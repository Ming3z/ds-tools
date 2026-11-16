package cli

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dstools/internal/deepseek"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ocrJSON(pages ...string) string {
	items := make([]string, len(pages))
	for i, p := range pages {
		items[i] = fmt.Sprintf(`{"index":%d,"text":%q,"finish_reason":"stop"}`, i, p)
	}
	return fmt.Sprintf(`{"id":"ocr-1","object":"ocr.result","created":1,"purpose":"text_recognition","page_count":%d,"pages":[%s],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
		len(pages), strings.Join(items, ","))
}

func readCLIUpload(t *testing.T, r *http.Request) (purpose, name string, body []byte) {
	t.Helper()
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	purpose = r.FormValue("purpose")
	f, hdr, err := r.FormFile("file")
	if err != nil {
		t.Fatalf("FormFile: %v", err)
	}
	defer func() { _ = f.Close() }()
	body, err = io.ReadAll(f)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return purpose, hdr.Filename, body
}

func testWAV(sampleRate uint32, channels, bits uint16, seconds float64) []byte {
	return testWAVFormat(sampleRate, channels, bits, 1, seconds)
}

func testWAVFormat(sampleRate uint32, channels, bits, audioFormat uint16, seconds float64) []byte {
	dataSize := int(float64(sampleRate) * float64(channels) * float64(bits) / 8 * seconds)
	if dataSize%2 == 1 {
		dataSize++
	}
	buf := make([]byte, 44+dataSize)
	copy(buf[0:], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataSize))
	copy(buf[8:], "WAVE")
	copy(buf[12:], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], audioFormat)
	binary.LittleEndian.PutUint16(buf[22:], channels)
	binary.LittleEndian.PutUint32(buf[24:], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:], sampleRate*uint32(channels)*uint32(bits)/8)
	binary.LittleEndian.PutUint16(buf[32:], channels*bits/8)
	binary.LittleEndian.PutUint16(buf[34:], bits)
	copy(buf[36:], "data")
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataSize))
	return buf
}

func fakeWAV(payload int) []byte {
	b := make([]byte, 44+payload)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+payload))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 24000)
	binary.LittleEndian.PutUint32(b[28:], 48000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(payload))
	return b
}

func TestOCRDefaultTextJSONLAndCache(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/ocr" {
			t.Errorf("path = %s", r.URL.Path)
		}
		fmt.Fprint(w, ocrJSON("first", "second"))
	}
	path := writeTemp(t, "doc.pdf", []byte("pdf"))

	if code := h.run("ocr", path); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if got := h.out.String(); got != "first\n\nsecond\n" {
		t.Fatalf("text = %q", got)
	}

	// The second call is served from the content-hash cache.
	h.out.Reset()
	if code := h.run("ocr", path, "--page", "2", "--format", "jsonl"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if h.hits != 1 {
		t.Fatalf("cache miss: hits = %d", h.hits)
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"text":"second"`) {
		t.Fatalf("jsonl = %q", h.out.String())
	}

	// --no-cache must re-request.
	h.out.Reset()
	if code := h.run("ocr", path, "--format", "json", "--no-cache"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.hits != 2 {
		t.Fatalf("--no-cache hits = %d", h.hits)
	}
}

func TestOCRMultipartPurposeAndJSONFields(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		purpose, name, body := readCLIUpload(t, r)
		if purpose != "rich_layout_recognition" {
			t.Errorf("purpose = %q", purpose)
		}
		if name != "doc.pdf" || string(body) != "pdf" {
			t.Errorf("file = %q (%q)", name, body)
		}
		fmt.Fprint(w, ocrJSON("first", "second"))
	}
	path := writeTemp(t, "doc.pdf", []byte("pdf"))

	if code := h.run("ocr", path, "--purpose", "rich_layout_recognition", "--format", "json", "--fields", "text"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	v := h.jsonOut(t)
	if len(v) != 1 || v["text"] != "first\n\nsecond" {
		t.Fatalf("projected json = %v", v)
	}
}

func TestOCRInBodyError(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":{"message":"OCR did not finish in time","type":"gateway_timeout_error","param":null,"code":"gateway_timeout_error"}}`)
	}
	path := writeTemp(t, "doc.pdf", []byte("pdf"))
	if code := h.run("ocr", path); code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	if !strings.Contains(h.err.String(), "did not finish") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestOCRSizeLimitAndBadPageSpecNeverHitNetwork(t *testing.T) {
	h := newHarness(t)
	path := writeTemp(t, "doc.pdf", []byte("pdf"))

	if code := h.run("ocr", path, "--page", "0"); code != ExitUsage {
		t.Fatalf("bad page exit = %d", code)
	}
	if h.hits != 0 {
		t.Fatalf("bad page hit the network")
	}

	big := filepath.Join(t.TempDir(), "big.pdf")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(100<<20 + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if code := h.run("ocr", big); code != ExitUsage {
		t.Fatalf("oversize exit = %d", code)
	}
	if h.hits != 0 {
		t.Fatalf("oversize file hit the network")
	}
}

func TestOCRDryRun(t *testing.T) {
	h := newHarness(t)
	path := writeTemp(t, "doc.pdf", []byte("pdf"))
	if code := h.run("ocr", path, "--dry-run"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if h.hits != 0 {
		t.Fatal("dry-run made a request")
	}
	out := h.out.String()
	for _, want := range []string{"/v1/tools/ocr", "purpose=text_recognition", "file=doc.pdf"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run missing %q:\n%s", want, out)
		}
	}
}

func TestASRDefaultTextJSONFieldsAndCache(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/asr" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, name, body := readCLIUpload(t, r)
		if name != "speech.wav" || len(body) == 0 {
			t.Errorf("file = %q (%d bytes)", name, len(body))
		}
		fmt.Fprint(w, `{"request_id":"r1","text":"上海今天天气怎么样？","duration_ms":3200}`)
	}
	path := writeTemp(t, "speech.wav", testWAV(16000, 1, 16, 0.1))

	if code := h.run("asr", path); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if h.out.String() != "上海今天天气怎么样？\n" {
		t.Fatalf("text = %q", h.out.String())
	}

	h.out.Reset()
	if code := h.run("asr", path, "--format", "json", "--fields", "text"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.hits != 1 {
		t.Fatalf("cache miss: hits = %d", h.hits)
	}
	if got := strings.TrimSpace(h.out.String()); got != `{"text":"上海今天天气怎么样？"}` {
		t.Fatalf("json = %s", got)
	}

	h.out.Reset()
	if code := h.run("asr", path, "--format", "json", "--no-cache"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if h.hits != 2 {
		t.Fatalf("--no-cache hits = %d", h.hits)
	}
}

func TestASRRejectsInvalidAudio(t *testing.T) {
	cases := map[string][]byte{
		"not audio":   []byte("not audio"),
		"wav 8-bit":   testWAV(16000, 1, 8, 0.1),
		"wav 121s":    testWAV(8000, 1, 16, 121),
		"ogg no opus": append([]byte("OggS"), make([]byte, 40)...),
	}
	for name, data := range cases {
		h := newHarness(t)
		path := writeTemp(t, "audio.bin", data)
		if code := h.run("asr", path); code != ExitUsage {
			t.Fatalf("%s: exit = %d, want 4 (stderr=%s)", name, code, h.err.String())
		}
		if h.hits != 0 {
			t.Fatalf("%s: invalid audio hit the network", name)
		}
	}
}

func TestASROggOpusAccepted(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		_, _, body := readCLIUpload(t, r)
		if string(body[:4]) != "OggS" {
			t.Errorf("body = %q", body[:4])
		}
		fmt.Fprint(w, `{"request_id":"r","text":"ok","duration_ms":1000}`)
	}
	ogg := append([]byte("OggS"), make([]byte, 40)...)
	copy(ogg[10:], "OpusHead")
	path := writeTemp(t, "speech.ogg", ogg)
	if code := h.run("asr", path); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
}

func TestASRAcceptsExtensiblePCM(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"request_id":"r","text":"ok","duration_ms":1000}`)
	}
	path := writeTemp(t, "speech.wav", testWAVFormat(16000, 1, 16, 0xFFFE, 0.1))
	if code := h.run("asr", path); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
}

func TestASRDryRun(t *testing.T) {
	h := newHarness(t)
	path := writeTemp(t, "speech.wav", testWAV(16000, 1, 16, 0.1))
	if code := h.run("asr", path, "--dry-run"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if h.hits != 0 {
		t.Fatal("dry-run made a request")
	}
	if !strings.Contains(h.out.String(), "/v1/tools/asr") || !strings.Contains(h.out.String(), "file=speech.wav") {
		t.Fatalf("dry-run = %s", h.out.String())
	}
}

func TestTTSFileAndMetadata(t *testing.T) {
	audio := fakeWAV(4)
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/tts" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req["text"] != "hello" || req["voice_id"] != "stella" || req["format"] != "wav" || req["filter_markdown"] != true {
			t.Errorf("req = %v", req)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(audio)
	}
	out := filepath.Join(t.TempDir(), "hello.wav")
	if code := h.run("tts", "hello", "-o", out, "--format", "json"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(audio) {
		t.Fatalf("audio = %q", got)
	}
	var meta map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &meta); err != nil {
		t.Fatalf("metadata not JSON: %v (%s)", err, h.out.String())
	}
	if meta["output"] != out || meta["bytes"] != float64(len(audio)) || meta["voice_id"] != "stella" || meta["audio_format"] != "wav" || meta["billing_chars"] != float64(5) {
		t.Fatalf("metadata = %v", meta)
	}
}

func TestTTSStdoutAndKeepMarkdown(t *testing.T) {
	audio := fakeWAV(4)
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["filter_markdown"] != false {
			t.Errorf("filter_markdown = %v", req["filter_markdown"])
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(audio)
	}
	if code := h.run("tts", "hello", "--keep-markdown"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if string(h.out.Bytes()) != string(audio) {
		t.Fatalf("stdout is not the raw audio (%d bytes)", h.out.Len())
	}
}

func TestTTSValidationNeverHitsNetwork(t *testing.T) {
	cases := [][]string{
		{"tts", ""},
		{"tts", strings.Repeat("汉", 5001)},
		{"tts", "hello", "--voice", "nope"},
		{"tts", "hello", "--audio-format", "mp3"},
		{"tts", "hello", "--format", "md"},
		{"tts", "hello", "--fields", "text"},
	}
	for _, args := range cases {
		h := newHarness(t)
		if code := h.run(args...); code != ExitUsage {
			t.Fatalf("%v -> exit %d, want 4 (stderr=%s)", args, code, h.err.String())
		}
		if h.hits != 0 {
			t.Fatalf("%v hit the network", args)
		}
	}
}

func TestTTSDryRun(t *testing.T) {
	h := newHarness(t)
	if code := h.run("tts", "hello", "--dry-run", "--format", "json"); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, h.err.String())
	}
	if h.hits != 0 {
		t.Fatal("dry-run made a request")
	}
	out := h.out.String()
	for _, want := range []string{"/v1/tools/tts", `"text": "hello"`, `"voice_id": "stella"`, `"format": "wav"`, `"filter_markdown": true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run missing %q:\n%s", want, out)
		}
	}
}

func TestTTSMidStreamFailureLeavesNoFile(t *testing.T) {
	h := newHarness(t)
	h.handle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write(fakeWAV(0)[:12])
	}
	out := filepath.Join(t.TempDir(), "broken.wav")
	if code := h.run("tts", "hello", "-o", out, "--format", "json"); code != ExitServer {
		t.Fatalf("exit = %d, want %d (stderr=%s)", code, ExitServer, h.err.String())
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("partial audio file was left behind: %v", err)
	}
}

func TestParsePageSpecAndSelect(t *testing.T) {
	cases := map[string][]pageRange{
		"":      nil,
		"1-3,5": {{1, 3}, {5, 5}},
		"5-":    {{5, 0}},
		"-3":    {{1, 3}},
	}
	for spec, want := range cases {
		got, err := parsePageSpec(spec)
		if err != nil {
			t.Fatalf("%q: %v", spec, err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%q -> %v, want %v", spec, got, want)
		}
	}
	for _, bad := range []string{"0", "-", "3-1", "a", "1,"} {
		if _, err := parsePageSpec(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}

	pages := []deepseek.OCRPage{
		{Index: 0, Text: "a"},
		{Index: 1, Text: "b"},
		{Index: 2, Text: "c"},
		{Index: 3, Text: "d"},
	}
	got := selectOCRPages(pages, []pageRange{{1, 2}, {4, 0}})
	if len(got) != 3 || got[0].Index != 0 || got[1].Index != 1 || got[2].Index != 3 {
		t.Fatalf("selected = %+v", got)
	}
	if got := selectOCRPages(pages, []pageRange{{10, 12}}); len(got) != 0 {
		t.Fatalf("out-of-range selection = %+v", got)
	}
}

func TestTTSBillingChars(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"hello": 5,
		"你好":    4,
		"a你":    3,
		"😀":     1,
	}
	for in, want := range cases {
		if got := ttsBillingChars(in); got != want {
			t.Fatalf("ttsBillingChars(%q) = %d, want %d", in, got, want)
		}
	}
}
