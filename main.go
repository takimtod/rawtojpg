package main

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	pageURL  = "https://www.iloveimg.com/convert-to-jpg/raw-to-jpg"
	apiURL   = "https://api5.iloveimg.com/v1"
	maxBytes = 200 << 20
)

var (
	configRE = regexp.MustCompile(`var ilovepdfConfig\s*=\s*(\{.*?\});`)
	rawExts  = map[string]bool{
		".arw": true, ".cr2": true, ".cr3": true, ".dng": true,
		".nef": true, ".nrw": true, ".orf": true, ".pef": true,
		".raf": true, ".rw2": true, ".srw": true,
	}
)

//go:embed index.html
var indexHTML []byte

type pageConfig struct {
	Token   string   `json:"token"`
	Servers []string `json:"servers"`
	Tool    string   `json:"tool"`
	Subtool string   `json:"subtool"`
}

type uploadResult struct {
	ServerFilename string `json:"server_filename"`
}

type taskResult struct {
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(indexHTML))
	})
	mux.HandleFunc("POST /api/convert", convertHandler)

	addr := ":8080"
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		addr = ":" + strings.TrimPrefix(port, ":")
	}
	fmt.Printf("RAW to JPG berjalan di http://localhost%s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Printf("Server berhenti: %v\n", err)
	}
}

func convertHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "File terlalu besar atau formulir tidak valid (maksimal 200 MB).", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Pilih file RAW untuk dikonversi.", http.StatusBadRequest)
		return
	}
	defer file.Close()
	filename := safeFilename(header.Filename)
	if !rawExts[strings.ToLower(path.Ext(filename))] {
		http.Error(w, "Format RAW tidak didukung. Gunakan CR2, CR3, NEF, ARW, DNG, RAF, RW2, ORF, PEF, atau SRW.", http.StatusUnsupportedMediaType)
		return
	}

	input, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(input) == 0 || len(input) > maxBytes {
		http.Error(w, "File kosong, tidak dapat dibaca, atau melebihi 200 MB.", http.StatusBadRequest)
		return
	}

	result, err := convertRAW(r.Context(), input, filename)
	if err != nil {
		fmt.Printf("Konversi %q gagal: %v\n", filename, err)
		http.Error(w, "Konversi gagal. Coba lagi beberapa saat.", http.StatusBadGateway)
		return
	}
	outputName := strings.TrimSuffix(filename, path.Ext(filename)) + ".jpg"
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": outputName}))
	w.Header().Set("Content-Length", fmt.Sprint(len(result)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func convertRAW(parent context.Context, input []byte, filename string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 3 * time.Minute, Jar: jar}
	config, err := getPageConfig(ctx, client)
	if err != nil {
		return nil, err
	}
	if config.Token == "" || config.Tool != "convertimage" || config.Subtool != "tojpg" {
		return nil, errors.New("converter config is missing")
	}
	available := false
	for _, server := range config.Servers {
		if server == "api5" {
			available = true
			break
		}
	}
	if !available {
		return nil, errors.New("upload server is unavailable")
	}
	taskID, err := newTaskID()
	if err != nil {
		return nil, err
	}
	defer deleteTask(client, config.Token, taskID)

	uploaded, err := upload(ctx, client, config.Token, taskID, input, filename)
	if err != nil {
		return nil, err
	}
	if err := process(ctx, client, config.Token, taskID, uploaded.ServerFilename, filename); err != nil {
		return nil, err
	}
	if err := waitForTask(ctx, client, config.Token, taskID, uploaded.ServerFilename, filename); err != nil {
		return nil, err
	}
	return download(ctx, client, config.Token, taskID)
}

func getPageConfig(ctx context.Context, client *http.Client) (pageConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return pageConfig{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return pageConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pageConfig{}, fmt.Errorf("converter page returned %s", resp.Status)
	}
	html, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return pageConfig{}, err
	}
	match := configRE.FindSubmatch(html)
	if len(match) != 2 {
		return pageConfig{}, errors.New("converter config not found")
	}
	var config pageConfig
	err = json.Unmarshal(match[1], &config)
	return config, err
}

func upload(ctx context.Context, client *http.Client, token, taskID string, input []byte, filename string) (uploadResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range map[string]string{"task": taskID, "preview": "0"} {
		if err := writer.WriteField(key, value); err != nil {
			return uploadResult{}, err
		}
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return uploadResult{}, err
	}
	if _, err := part.Write(input); err != nil {
		return uploadResult{}, err
	}
	if err := writer.Close(); err != nil {
		return uploadResult{}, err
	}
	resp, err := apiRequest(ctx, client, http.MethodPost, apiURL+"/upload", token, writer.FormDataContentType(), &body)
	if err != nil {
		return uploadResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return uploadResult{}, fmt.Errorf("upload returned %s", resp.Status)
	}
	var result uploadResult
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	if err == nil && result.ServerFilename == "" {
		err = errors.New("upload response has no server filename")
	}
	return result, err
}

func process(ctx context.Context, client *http.Client, token, taskID, serverFilename, filename string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"task": taskID, "tool": "convertimage", "convert_to": "jpg",
		"files[0][server_filename]": serverFilename, "files[0][filename]": filename, "files[0][pages]": "1",
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	resp, err := apiRequest(ctx, client, http.MethodPost, apiURL+"/process", token, writer.FormDataContentType(), &body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("process returned %s", resp.Status)
	}
	return nil
}

func waitForTask(ctx context.Context, client *http.Client, token, taskID, serverFilename, filename string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		resp, err := apiRequest(ctx, client, http.MethodGet, apiURL+"/task/"+taskID, token, "", nil)
		if err != nil {
			return err
		}
		var status taskResult
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status)
		resp.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("task status returned %s", resp.Status)
		}
		switch status.Status {
		case "TaskSuccess", "TaskSuccessWithWarnings":
			return nil
		case "TaskWaiting":
			if err := process(ctx, client, token, taskID, serverFilename, filename); err != nil {
				return err
			}
		case "TaskProcessing":
		default:
			return fmt.Errorf("conversion task failed: %s", status.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func download(ctx context.Context, client *http.Client, token, taskID string) ([]byte, error) {
	resp, err := apiRequest(ctx, client, http.MethodGet, apiURL+"/download/"+taskID, token, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxBytes {
		return nil, errors.New("invalid converted file size")
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "image/jpeg" || len(data) < 3 || !bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}) {
		return nil, fmt.Errorf("converter returned %q instead of JPEG", mediaType)
	}
	return data, nil
}

func apiRequest(ctx context.Context, client *http.Client, method, url, token, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://www.iloveimg.com")
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return client.Do(req)
}

func deleteTask(client *http.Client, token, taskID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := apiRequest(ctx, client, http.MethodDelete, apiURL+"/task/"+taskID, token, "", nil)
	if err == nil {
		resp.Body.Close()
	}
}

func newTaskID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], data[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], data[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], data[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], data[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], data[10:16])
	return string(encoded), nil
}

func safeFilename(filename string) string {
	filename = path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if filename == "." || filename == "/" || filename == "" {
		return "image.raw"
	}
	return filename
}
