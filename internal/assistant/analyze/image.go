package analyze

import (
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"sort"
)

// HueShare is one dominant hue: degrees 0..360 (bin centre) and its share of
// the coloured pixels' picture area.
type HueShare struct {
	Hue   float64 `json:"hue"`
	Share float64 `json:"share"`
}

// ImageMetrics is the measurement of one picture.
type ImageMetrics struct {
	Width, Height int
	// Luma32, R32, G32, B32 are 32-bin histograms, each summing to 1.
	Luma32, R32, G32, B32 [32]float64
	// ShadowsClipped/HighlightsClipped are the shares of pixels at the
	// bottom (luma <= 1/255) and top (>= 254/255) of the range.
	ShadowsClipped, HighlightsClipped float64
	MeanLuma, MedianLuma, LumaStd     float64
	// Warmth is the grey-world blue-to-red balance ((R-B)/mean(R,G,B));
	// Tint is the green-magenta balance ((G-(R+B)/2)/mean). 0 = neutral.
	Warmth, Tint float64
	// Sharpness is the variance of the Laplacian (higher = crisper).
	Sharpness float64
	// NoiseSigma is the luma standard deviation (0..1) of the flattest blocks.
	NoiseSigma float64
	// DominantHues holds at most 5 hues of the coloured pixels.
	DominantHues []HueShare
}

const maxAnalyzeSide = 512

// AnalyzeImage decodes a JPEG, PNG or GIF and measures it. Large pictures
// are sampled down to 512 px on the long side first.
func AnalyzeImage(r io.Reader) (*ImageMetrics, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() < 1 || b.Dy() < 1 {
		return nil, errors.New("analyze: empty image")
	}
	step := 1
	for long := max(b.Dx(), b.Dy()); long/step > maxAnalyzeSide; step++ {
	}
	w, h := (b.Dx()+step-1)/step, (b.Dy()+step-1)/step
	rgb := make([][3]float64, w*h) // 0..255, sRGB-encoded
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x*step, b.Min.Y+y*step).RGBA()
			if a == 0 {
				rgb[y*w+x] = [3]float64{}
				continue
			}
			// un-premultiply, 16 → 8 bit
			rgb[y*w+x] = [3]float64{float64(r) / float64(a) * 255, float64(g) / float64(a) * 255, float64(bl) / float64(a) * 255}
		}
	}
	return measure(rgb, w, h), nil
}

func luma709(p [3]float64) float64 { return 0.2126*p[0] + 0.7152*p[1] + 0.0722*p[2] }

func measure(rgb [][3]float64, w, h int) *ImageMetrics {
	m := &ImageMetrics{Width: w, Height: h}
	n := float64(len(rgb))
	var hist [256]float64
	luma := make([]float64, len(rgb))
	var sum, sumSq float64
	var sr, sg, sb float64
	var lo, hi float64
	for i, p := range rgb {
		l := luma709(p)
		luma[i] = l
		li := int(math.Round(l))
		hist[li]++
		sum += l
		sumSq += l * l
		sr, sg, sb = sr+p[0], sg+p[1], sb+p[2]
		if li <= 1 {
			lo++
		}
		if li >= 254 {
			hi++
		}
		m.Luma32[min(31, int(l)/8)]++
		m.R32[min(31, int(p[0])/8)]++
		m.G32[min(31, int(p[1])/8)]++
		m.B32[min(31, int(p[2])/8)]++
	}
	for i := range m.Luma32 {
		m.Luma32[i] = round(m.Luma32[i]/n, 4)
		m.R32[i] = round(m.R32[i]/n, 4)
		m.G32[i] = round(m.G32[i]/n, 4)
		m.B32[i] = round(m.B32[i]/n, 4)
	}
	m.ShadowsClipped, m.HighlightsClipped = round(lo/n, 4), round(hi/n, 4)
	mean := sum / n
	m.MeanLuma = round(mean/255, 4)
	m.LumaStd = round(math.Sqrt(math.Max(0, sumSq/n-mean*mean))/255, 4)
	acc := 0.0
	for i, c := range hist {
		acc += c
		if acc >= n/2 {
			m.MedianLuma = round(float64(i)/255, 4)
			break
		}
	}
	if avg := (sr + sg + sb) / (3 * n); avg > 0 {
		m.Warmth = round((sr/n-sb/n)/avg, 4)
		m.Tint = round((sg/n-(sr/n+sb/n)/2)/avg, 4)
	}
	m.Sharpness, m.NoiseSigma = sharpnessAndNoise(luma, w, h)
	m.DominantHues = dominantHues(rgb)
	return m
}

// sharpnessAndNoise: variance of the 4-neighbour Laplacian over the picture,
// and the mean luma std-dev of the flattest 10 % of 8×8 blocks (blocks whose
// gradients are smallest, so texture is not read as noise).
func sharpnessAndNoise(luma []float64, w, h int) (sharp, noise float64) {
	if w < 3 || h < 3 {
		return 0, 0
	}
	var s, s2 float64
	cnt := 0.0
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			l := luma[(y-1)*w+x] + luma[(y+1)*w+x] + luma[y*w+x-1] + luma[y*w+x+1] - 4*luma[y*w+x]
			s += l
			s2 += l * l
			cnt++
		}
	}
	mean := s / cnt
	sharp = round(math.Max(0, s2/cnt-mean*mean), 2)

	type blk struct{ grad, std float64 }
	var blocks []blk
	for by := 0; by+8 <= h; by += 8 {
		for bx := 0; bx+8 <= w; bx += 8 {
			var bs, bs2, grad float64
			for y := 0; y < 8; y++ {
				for x := 0; x < 8; x++ {
					v := luma[(by+y)*w+bx+x]
					bs += v
					bs2 += v * v
					if x > 0 {
						grad += math.Abs(v - luma[(by+y)*w+bx+x-1])
					}
					if y > 0 {
						grad += math.Abs(v - luma[(by+y-1)*w+bx+x])
					}
				}
			}
			bm := bs / 64
			blocks = append(blocks, blk{grad, math.Sqrt(math.Max(0, bs2/64-bm*bm))})
		}
	}
	if len(blocks) == 0 {
		return sharp, 0
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].grad < blocks[j].grad })
	k := max(1, len(blocks)/10)
	var t float64
	for _, b := range blocks[:k] {
		t += b.std
	}
	return sharp, round(t/float64(k)/255, 4)
}

// dominantHues bins the coloured pixels (saturation > 0.2, value > 0.15) into
// 36 hue bins of 10° and returns the top five with share >= 2 % of all pixels.
func dominantHues(rgb [][3]float64) []HueShare {
	var bins [36]float64
	for _, p := range rgb {
		r, g, b := p[0]/255, p[1]/255, p[2]/255
		mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
		d := mx - mn
		if mx < 0.15 || d/mx < 0.2 {
			continue
		}
		var hue float64
		switch mx {
		case r:
			hue = math.Mod((g-b)/d, 6)
		case g:
			hue = (b-r)/d + 2
		default:
			hue = (r-g)/d + 4
		}
		hue *= 60
		if hue < 0 {
			hue += 360
		}
		bins[int(hue/10)%36]++
	}
	idx := make([]int, 36)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return bins[idx[a]] > bins[idx[b]] })
	var out []HueShare
	n := float64(len(rgb))
	for _, i := range idx[:5] {
		if share := bins[i] / n; share >= 0.02 {
			out = append(out, HueShare{Hue: float64(i*10 + 5), Share: round(share, 4)})
		}
	}
	return out
}

// Set flattens the metrics to the shared MetricSet names (the browser sends a
// subset of these).
func (m *ImageMetrics) Set() MetricSet {
	s := MetricSet{
		"meanLuma": m.MeanLuma, "medianLuma": m.MedianLuma, "lumaStd": m.LumaStd,
		"shadowsClipped": m.ShadowsClipped, "highlightsClipped": m.HighlightsClipped,
		"warmth": m.Warmth, "tint": m.Tint, "sharpness": m.Sharpness, "noiseSigma": m.NoiseSigma,
	}
	if len(m.DominantHues) > 0 {
		s["dominantHue"] = m.DominantHues[0].Hue
		s["dominantHueShare"] = m.DominantHues[0].Share
	}
	return s
}
