package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The pages every kind of target shows (the same HTML in desktop Chrome,
// Chrome on the emulator and Safari on the simulator), served by the
// harness on 127.0.0.1. Scrollbars are hidden: their fading out after a
// scroll would be taken for the next input's change.

const pageHead = `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title><style>
html{scrollbar-width:none} ::-webkit-scrollbar{display:none}
body{margin:0;font:18px/1.6 -apple-system,Roboto,"Helvetica Neue",sans-serif;color:#222;background:#fafafa}
main{max-width:900px;margin:0 auto;padding:16px}
h1,h2{line-height:1.2} .card{border-radius:12px;padding:16px;margin:16px 0;color:#fff}
textarea{box-sizing:border-box;width:100%%;height:38vh;font-size:32px;caret-color:transparent;
  border:2px solid #888;border-radius:8px;padding:8px;resize:none}
video{width:100%%;display:block;background:#000}
</style></head><body><main>`

const pageTail = `</main></body></html>`

var cardColors = []string{"#3b6fd6", "#d6513b", "#2f9e6a", "#9b4fd1", "#c79a1c", "#1c8fc7"}

func paragraph(i int) string {
	return fmt.Sprintf(`<p>%d. The host screen tab shows a browser, an Android emulator or an iOS simulator
running on the host, one JPEG at a time over the tab's own SSH connection. This paragraph is
filler so that the page has text of ordinary density to scroll through; each one is numbered so
that two scroll positions never look alike.</p>`, i)
}

// pages returns path → HTML.
func pages() map[string]string {
	var still, scroll strings.Builder
	still.WriteString(fmt.Sprintf(pageHead, "still"))
	still.WriteString(`<h1>Still screen</h1>`)
	for i := range 3 {
		still.WriteString(paragraph(i + 1))
		still.WriteString(fmt.Sprintf(`<div class="card" style="background:%s">Card %d</div>`, cardColors[i], i+1))
	}
	still.WriteString(pageTail)

	scroll.WriteString(fmt.Sprintf(pageHead, "scroll"))
	scroll.WriteString(`<h1>Scroll</h1>`)
	for i := range 240 {
		if i%4 == 0 {
			scroll.WriteString(fmt.Sprintf(`<h2>Section %d</h2>`, i/4+1))
		}
		scroll.WriteString(paragraph(i + 1))
		if i%3 == 2 {
			scroll.WriteString(fmt.Sprintf(`<div class="card" style="background:%s">Card %d</div>`, cardColors[i%len(cardColors)], i/3+1))
		}
	}
	scroll.WriteString(pageTail)

	text := fmt.Sprintf(pageHead, "text") +
		`<textarea autofocus autocomplete="off" autocorrect="off" autocapitalize="off" spellcheck="false"></textarea>` +
		paragraph(1) + pageTail

	video := fmt.Sprintf(pageHead, "video") +
		`<video src="/clip.mp4" autoplay muted loop playsinline></video>` + paragraph(1) + pageTail

	return map[string]string{"/still": still.String(), "/scroll": scroll.String(), "/text": text, "/video": video}
}

// videoArgs make the looping clip of the video scenario: ffmpeg's testsrc2
// pattern (moving colour bars, a counter and gradients), 1280×720 30 fps,
// 10 s of H.264 that every browser here plays. Generated, so that a rerun
// shows the same video without a binary in the repository.
func videoArgs(out string) []string {
	return []string{"-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30",
		"-t", "10", "-c:v", "libx264", "-preset", "medium", "-crf", "23", "-pix_fmt", "yuv420p",
		"-profile:v", "main", "-movflags", "+faststart", out}
}

// ensureVideo makes the clip in dir unless it is there; "" when ffmpeg is
// missing (the video scenario is then skipped).
func ensureVideo(dir string) (string, error) {
	out := filepath.Join(dir, "clip.mp4")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := out + ".tmp.mp4"
	if b, err := exec.Command(ffmpeg, videoArgs(tmp)...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg: %v\n%s", err, b)
	}
	return out, os.Rename(tmp, out)
}

// servePages serves the pages (and the clip, when there is one) and returns
// the port.
func servePages(clip string) (int, error) {
	mux := http.NewServeMux()
	for path, html := range pages() {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(html))
		})
	}
	if clip != "" {
		mux.HandleFunc("/clip.mp4", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, clip) })
	}
	// The simulator shares the host's loopback; the emulator reaches it as
	// 10.0.2.2.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	go http.Serve(ln, mux)
	return ln.Addr().(*net.TCPAddr).Port, nil
}
