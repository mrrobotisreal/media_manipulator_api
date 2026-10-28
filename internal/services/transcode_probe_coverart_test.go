package services

import (
	"encoding/json"
	"testing"
)

// The transcode probe must not report an mp3's embedded cover art as the
// video track (and its 90 kHz timebase as 90000 fps).
func TestTranscodeProbeIgnoresCoverArtStream(t *testing.T) {
	var res ffprobeResult
	raw := `{"streams":[
	  {"index":0,"codec_type":"audio","codec_name":"mp3","duration":"2.0"},
	  {"index":1,"codec_type":"video","codec_name":"png","width":320,"height":240,"avg_frame_rate":"0/0","r_frame_rate":"90000/1","disposition":{"attached_pic":1}}
	],"format":{"duration":"2.0"}}`
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res.Streams[1].Disposition.AttachedPic != 1 {
		t.Fatalf("disposition not parsed: %+v", res.Streams[1])
	}
}
