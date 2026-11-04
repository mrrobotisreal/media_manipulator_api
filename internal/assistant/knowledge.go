package assistant

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

// EmbeddedKnowledge is MM105's Knowledge: the generated corpus parsed once,
// a BM25 index, the per-surface capsules and the guide's scope gate.
type EmbeddedKnowledge struct {
	corpus   *Corpus
	index    *bm25Index
	capsules map[string]string
	// scopeTerms is every distinctive token of titles, keywords and names.
	scopeTerms map[string]bool
	// scopePhrases are multi-word keywords matched as substrings.
	scopePhrases []string
}

var (
	embeddedOnce sync.Once
	embeddedK    *EmbeddedKnowledge
	embeddedErr  error
)

// NewEmbeddedKnowledge loads the go:embed corpus (once per process). An error
// means the export is malformed; the wiring logs it and falls back to the stub.
func NewEmbeddedKnowledge() (*EmbeddedKnowledge, error) {
	embeddedOnce.Do(func() { embeddedK, embeddedErr = NewKnowledgeFrom(knowledgeFS, "knowledge") })
	return embeddedK, embeddedErr
}

// NewKnowledgeFrom builds the knowledge base from any knowledge tree (tests).
func NewKnowledgeFrom(fsys fs.FS, root string) (*EmbeddedKnowledge, error) {
	c, err := LoadCorpus(fsys, root)
	if err != nil {
		return nil, err
	}
	if len(c.Docs) == 0 {
		return nil, fmt.Errorf("knowledge: no documents under %s", root)
	}
	k := &EmbeddedKnowledge{corpus: c, index: newBM25(c.Docs)}
	k.buildScope()
	k.capsules = map[string]string{
		SurfaceGuide:  k.buildGuideCapsule(),
		SurfaceStudio: k.buildEditorCapsule(SurfaceStudio),
		SurfaceVector: k.buildEditorCapsule(SurfaceVector),
		SurfacePhoto:  k.buildEditorCapsule(SurfacePhoto),
	}
	return k, nil
}

// Corpus exposes the parsed docs (counts, tests, MM106's eval harness).
func (k *EmbeddedKnowledge) Corpus() *Corpus { return k.corpus }

// visible reports whether a doc may be retrieved on a surface (decision 5):
// guide → tools, pages and workflow recipes; a copilot → its editor docs,
// tools and its own (and workflow) recipes.
func visible(d CorpusDoc, surface string) bool {
	switch d.Kind {
	case "tool":
		return true
	case "page":
		return surface == SurfaceGuide
	case "editor":
		return d.Category == surface
	case "recipe":
		if d.Category == "workflow" {
			return true
		}
		return d.Category == surface
	}
	return false
}

// Search ranks the surface's docs for q with BM25 (aliases expanded) and
// returns the top k with a best-window snippet.
func (k *EmbeddedKnowledge) Search(q, surface string, n int) []Hit {
	if n <= 0 {
		return nil
	}
	terms := uniqueTerms(tokenize(expandQuery(q)))
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		i int
		s float64
	}
	var all []scored
	for i, d := range k.index.docs {
		if !visible(d, surface) {
			continue
		}
		if s := k.index.score(i, terms); s > 0 {
			all = append(all, scored{i, s})
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].s != all[b].s {
			return all[a].s > all[b].s
		}
		return k.index.docs[all[a].i].ID < k.index.docs[all[b].i].ID
	})
	if len(all) > n {
		all = all[:n]
	}
	hits := make([]Hit, 0, len(all))
	for _, s := range all {
		d := k.index.docs[s.i]
		hits = append(hits, Hit{Doc: d.Doc, Snippet: snippet(d, terms), Score: s.s})
	}
	return hits
}

// Get returns a doc by id ("tools/compress-video", "recipes/photo/…").
func (k *EmbeddedKnowledge) Get(id string) (Doc, bool) {
	d, ok := k.corpus.Doc(strings.TrimSuffix(strings.Trim(id, "/ "), ".md"))
	return d.Doc, ok
}

// Capsule is the byte-stable block for a surface (decision 6).
func (k *EmbeddedKnowledge) Capsule(surface string) string { return k.capsules[surface] }

// Routes is routes.json.
func (k *EmbeddedKnowledge) Routes() *Routes { return k.corpus.routes }

// LocalName implements LocalNamer: a doc's name and path in the user's locale.
func (k *EmbeddedKnowledge) LocalName(id, locale string) (LocalName, bool) {
	return k.corpus.LocalName(id, locale)
}

// LocalNamer is the optional Knowledge extension the engine uses to name
// retrieved tools and pages in the user's language (decision 8).
type LocalNamer interface {
	LocalName(id, locale string) (LocalName, bool)
}

// ---------------------------------------------------------------------------
// Capsules
// ---------------------------------------------------------------------------

// capsuleChars estimates tokens the conservative way (chars / 3.5).
func CapsuleTokens(s string) int { return (len(s)*2 + 6) / 7 }

// MaxCapsuleTokens is decision 6's budget per capsule.
const MaxCapsuleTokens = 14000

var toolCategoryOrder = []struct{ id, title string }{
	{"contentStudio", "Darkroom video editor"},
	{"motion", "Motion SVG animation editor"},
	{"photo", "Aperture photo editor"},
	{"video", "Video tools"},
	{"image", "Image and PDF tools"},
	{"audio", "Audio tools"},
	{"ai", "AI tools"},
	{"metadata", "Privacy and metadata tools"},
}

// buildGuideCapsule is the site map (titles + routes, one line of what each
// tool does) and the plan overview, without any numbers.
func (k *EmbeddedKnowledge) buildGuideCapsule() string {
	var b strings.Builder
	b.WriteString("Site map. These are the only pages and tools that exist; link and navigate only to these routes (prefix them with the user's locale, e.g. /ru/tools/compress-video, when the live block names one).\n\n## Pages\n")
	for _, d := range k.corpus.Docs {
		if d.Kind == "page" {
			fmt.Fprintf(&b, "- %s: %s\n", d.Title, d.URL)
		}
	}
	b.WriteString("\n## Tools\n")
	for _, cat := range toolCategoryOrder {
		var lines []string
		for _, d := range k.corpus.Docs {
			if d.Kind == "tool" && d.Category == cat.id {
				lines = append(lines, fmt.Sprintf("- %s (%s): %s", d.Title, d.URL, firstSentence(d.Summary, 120)))
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n%s\n", cat.title, strings.Join(lines, "\n"))
	}
	if p, ok := k.corpus.Doc("pages/pricing"); ok {
		b.WriteString("\n## Plans (overview; the numbers are only in the live block)\n")
		b.WriteString(p.Summary)
		b.WriteString("\n")
		for _, line := range strings.Split(p.Details, "\n") {
			if strings.HasPrefix(line, "- ") {
				b.WriteString(line + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// buildEditorCapsule condenses the editor's capability docs (their parameter
// lists) plus, once MM106 lands, the editor's ten best-covering recipes.
func (k *EmbeddedKnowledge) buildEditorCapsule(surface string) string {
	name := map[string]string{SurfaceStudio: "Darkroom", SurfaceVector: "Motion", SurfacePhoto: "Aperture"}[surface]
	var b strings.Builder
	fmt.Fprintf(&b, "What %s can do (generated from the editor's code; use only these names and ranges):\n", name)
	for _, d := range k.corpus.Docs {
		if d.Kind != "editor" || d.Category != surface {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", d.Title, condense(d.Details))
	}
	recipes := k.topRecipes(surface, 10)
	if len(recipes) > 0 {
		b.WriteString("\n## Recipes (call get_recipe for the full version)\n")
		for _, d := range recipes {
			fmt.Fprintf(&b, "- %s [%s]: %s\n", d.Title, d.ID, firstSentence(d.Summary, 160))
		}
	}
	return strings.TrimSpace(b.String())
}

// topRecipes picks the n recipes of an editor with the most intents
// (keywords), ties by id: a stable "highest intent coverage" order.
func (k *EmbeddedKnowledge) topRecipes(surface string, n int) []CorpusDoc {
	var rs []CorpusDoc
	for _, d := range k.corpus.Docs {
		if d.Kind == "recipe" && d.Category == surface {
			rs = append(rs, d)
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if len(rs[i].Keywords) != len(rs[j].Keywords) {
			return len(rs[i].Keywords) > len(rs[j].Keywords)
		}
		return rs[i].ID < rs[j].ID
	})
	if len(rs) > n {
		rs = rs[:n]
	}
	return rs
}

// condense drops blank lines and collapses repeated spaces.
func condense(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " "))
		}
	}
	return strings.Join(out, "\n")
}

// firstSentence clips to the first sentence or n runes.
func firstSentence(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ". "); i > 0 && i < n {
		return s[:i+1]
	}
	return clip(s, n)
}

// ---------------------------------------------------------------------------
// Scope gate (guide only, decision 7)
// ---------------------------------------------------------------------------

// mediaCraftTerms always pass: general media questions are in scope even when
// no doc names them.
var mediaCraftTerms = setOf(`codec codecs bitrate bitrates aspect ratio resolution framerate fps frame frames colour color grade grading lut
export exporting caption captions subtitle subtitles keyframe keyframes video videos audio photo photos image images picture pictures
file files format formats mp4 mov mkv webm avi gif png jpg jpeg webp avif heic tiff svg pdf mp3 wav flac aac ogg opus hevc h264 h265
av1 vp9 prores hdr sdr lufs loudness podcast youtube instagram tiktok shorts reels thumbnail upload download convert compress edit
editing editor crop trim cut resize rotate filter effect effects transition transitions timeline layer layers mask masks animation
animate lottie vector watermark metadata exif transcribe transcription voice vocals music sound noise background render quality
media manipulator darkroom motion aperture medi tool tools plan plans pricing account premium pro sign login password billing
privacy terms refund delete history project projects quota limit limits error failed upload job jobs`)

// conversationalTerms let small talk through to the model (it answers in persona).
var conversationalTerms = setOf(`hi hello hey thanks thank ok okay yes no sure great cool help привет спасибо дякую hola gracias danke hallo merci bonjour grazie ciao
obrigado obrigada olá cảm ơn 你好 谢谢 謝謝 こんにちは ありがとう 안녕하세요 감사합니다 مرحبا شكرا नमस्ते धन्यवाद שלום תודה`)

// pageScopedPhrases pass (the visitor is asking about where they are).
var pageScopedPhrases = []string{
	"this page", "this tool", "this site", "on this site", "this editor",
	"эта страница", "этой странице", "этот сайт", "этом сайте", "этот инструмент",
	"ця сторінка", "цій сторінці", "цей сайт", "цьому сайті", "цей інструмент",
	"diese seite", "dieser seite", "dieses tool", "esta página", "este sitio", "esta herramienta",
	"cette page", "ce site", "cet outil", "questa pagina", "questo sito", "questo strumento",
	"このページ", "このサイト", "このツール", "这个页面", "这个网站", "这个工具", "這個頁面", "這個網站", "這個工具",
	"이 페이지", "이 사이트", "이 도구", "هذه الصفحة", "هذا الموقع", "هذه الأداة", "यह पेज", "इस पेज", "यह टूल",
	"este site", "esta ferramenta", "trang này", "công cụ này", "הדף הזה", "האתר הזה", "הכלי הזה",
	// Questions about Medi itself and the visitor's own things.
	"what can you do", "who are you", "what are you", "what do you do", "how do you work", "can you do",
	"my stuff", "my file", "my result", "my download", "my job", "my order", "my plan", "my account",
	"что ты умеешь", "кто ты", "що ти вмієш", "хто ти", "was kannst du", "wer bist du", "qué puedes hacer", "quién eres",
	"que peux-tu faire", "qui es-tu", "cosa puoi fare", "chi sei", "何ができ", "あなたは誰", "你能做什么", "你是谁", "你能做什麼", "你是誰",
	"뭘 할 수 있", "누구세요", "ماذا يمكنك", "من أنت", "तुम क्या कर सकते", "आप क्या कर सकते", "o que você pode", "quem é você",
	"bạn có thể làm gì", "bạn là ai", "מה אתה יכול", "מי אתה",
}

// genericScopeTokens appear in many docs but say nothing about scope alone
// (stored stemmed, the form tokenize produces).
var genericScopeTokens = stemmedSet(`page pages online free best new make made use used using way ways thing things one two good better fast easy
simple write writing tell know like look want help time work show give take find about guide guides tutorial picture`)

// siteVocabulary is the core of the site's vocabulary in the 17 other site
// languages (model-written, stems are enough): media nouns, editing verbs,
// plans and accounts. Tool and page names come from names/<locale>.json; this
// list catches questions phrased without a tool name ("Сколько стоит премиум?").
var siteVocabulary = `
видео фото фотографию изображение картинку аудио звук музыку файл файлы формат сжать сжатие конвертировать конвертер редактировать
редактор обрезать субтитры субтитров тариф тарифы цена стоит стоимость премиум подписка подписку аккаунт аккаунта лимит лимиты анимацию
відео фото зображення аудіо звук музику файл формат стиснути конвертувати редагувати редактор обрізати субтитри тариф ціна коштує
преміум підписка акаунт ліміт анімацію
video foto bild bilder audio ton musik datei dateien format komprimieren konvertieren bearbeiten schneiden untertitel preis preise kostet
abo abonnement konto limit animation
vídeo vídeos foto fotos imagen imágenes audio música archivo formato comprimir convertir editar recortar subtítulos precio precios
cuesta plan planes suscripción cuenta límite animación
vidéo vidéos photo photos image images audio musique fichier format compresser convertir modifier éditer couper sous-titres prix
coûte tarif abonnement compte limite animation
foto immagine immagini audio musica file formato comprimere convertire modificare tagliare sottotitoli prezzo prezzi costa piano
abbonamento account limite animazione
動画 ビデオ 写真 画像 音声 音楽 ファイル 形式 圧縮 変換 編集 トリミング 字幕 料金 価格 プラン プレミアム サブスク アカウント 上限 アニメーション
视频 照片 图片 图像 音频 音乐 文件 格式 压缩 转换 编辑 剪辑 字幕 价格 费用 套餐 高级 订阅 账户 限制 动画
影片 視訊 照片 圖片 影像 音訊 音樂 檔案 格式 壓縮 轉換 編輯 剪輯 字幕 價格 費用 方案 訂閱 帳號 限制 動畫
동영상 비디오 사진 이미지 오디오 음악 파일 형식 압축 변환 편집 자르기 자막 가격 요금 플랜 프리미엄 구독 계정 한도 애니메이션
فيديو صورة صور الصورة صوت موسيقى ملف تنسيق ضغط تحويل تعديل تحرير قص ترجمة سعر الأسعار تكلفة خطة اشتراك حساب حد رسوم متحركة
वीडियो फोटो तस्वीर इमेज ऑडियो संगीत फ़ाइल फाइल फ़ॉर्मेट कंप्रेस बदलें कन्वर्ट एडिट संपादन काटें सबटाइटल कीमत मूल्य प्लान प्रीमियम सदस्यता खाता सीमा एनिमेशन
vídeo vídeos foto fotografia imagem imagens áudio música ficheiro arquivo formato comprimir converter editar cortar legendas preço
custa plano assinatura subscrição conta limite animação
video ảnh hình âm thanh nhạc tệp định dạng nén chuyển đổi chỉnh sửa cắt phụ đề giá gói đăng ký tài khoản giới hạn hoạt hình
וידאו סרטון תמונה תמונות אודיו שמע מוזיקה קובץ פורמט לדחוס להמיר לערוך לחתוך כתוביות מחיר עולה תוכנית מנוי חשבון מגבלה אנימציה`

func (k *EmbeddedKnowledge) buildScope() {
	k.scopeTerms = map[string]bool{}
	seen := map[string]bool{}
	for _, d := range k.corpus.Docs {
		for _, field := range append(append([]string{d.Title}, d.Keywords...), d.Names...) {
			for _, t := range tokenize(field) {
				if len([]rune(t)) >= 3 && !genericScopeTokens[t] {
					k.scopeTerms[t] = true
				}
			}
			low := strings.ToLower(field)
			if strings.Contains(low, " ") && len(low) >= 6 && !seen[low] && distinctive(low) {
				seen[low] = true
				k.scopePhrases = append(k.scopePhrases, low)
			}
		}
	}
	for t := range mediaCraftTerms {
		for _, s := range tokenize(t) {
			k.scopeTerms[s] = true
		}
	}
	for _, s := range tokenize(siteVocabulary) {
		k.scopeTerms[s] = true
	}
	sort.Strings(k.scopePhrases)
}

// InScope is the guide's pre-model gate: a message passes when it names a
// doc keyword, an alias, a media-craft term, small talk, or the current page.
func (k *EmbeddedKnowledge) InScope(q string) bool {
	low := strings.ToLower(strings.TrimSpace(q))
	if low == "" {
		return false
	}
	for _, p := range pageScopedPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	if expandQuery(low) != low {
		return true // an alias matched
	}
	for _, p := range k.scopePhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	for _, w := range strings.FieldsFunc(low, func(r rune) bool { return r == ' ' || r == ',' || r == '!' || r == '?' || r == '.' }) {
		if conversationalTerms[w] {
			return true
		}
	}
	for _, t := range tokenize(low) {
		if k.scopeTerms[t] {
			return true
		}
	}
	return false
}

// distinctive reports whether a phrase has at least one token that is neither
// a stopword nor generic ("how to" is not; "lower third" is).
func distinctive(phrase string) bool {
	for _, t := range tokenize(phrase) {
		if len([]rune(t)) >= 3 && !genericScopeTokens[t] {
			return true
		}
	}
	return false
}

// stemmedSet is setOf over tokenize's output (stopwords dropped, stems kept).
func stemmedSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, t := range tokenize(words) {
		m[t] = true
	}
	return m
}

func setOf(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}
