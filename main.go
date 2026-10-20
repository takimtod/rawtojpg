package main

import (
	"archive/zip"
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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pageURL          = "https://www.iloveimg.com/convert-to-jpg/raw-to-jpg"
	apiURL           = "https://api5.iloveimg.com/v1"
	maxBytes         = 200 << 20
	maxFiles         = 20
	uploadChunkSize  = 8 << 20
	uploadSessionTTL = time.Hour
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
	uploads := newUploadManager()
	go uploads.cleanupLoop()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(indexHTML))
	})
	mux.HandleFunc("POST /api/convert", convertHandler)
	mux.HandleFunc("POST /api/upload", uploads.startHandler)
	mux.HandleFunc("PUT /api/upload/{id}/{index}", uploads.chunkHandler)
	mux.HandleFunc("DELETE /api/upload/{id}", uploads.cancelHandler)
	mux.HandleFunc("POST /api/convert/{id}", uploads.convertHandler)

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
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["files"]
		if len(files) == 0 {
			files = r.MultipartForm.File["file"]
		}
	}
	if len(files) == 0 {
		http.Error(w, "Pilih setidaknya satu file RAW untuk dikonversi.", http.StatusBadRequest)
		return
	}
	if len(files) > maxFiles {
		http.Error(w, "Maksimal 20 file RAW dapat dikonversi sekaligus.", http.StatusBadRequest)
		return
	}

	sources := make([]inputFile, 0, len(files))
	for _, header := range files {
		header := header
		sources = append(sources, inputFile{
			filename: header.Filename,
			size:     header.Size,
			open: func() (io.ReadCloser, error) {
				return header.Open()
			},
		})
	}
	convertFiles(w, r, sources)
}

type inputFile struct {
	filename string
	size     int64
	open     func() (io.ReadCloser, error)
}

func convertFiles(w http.ResponseWriter, r *http.Request, files []inputFile) {
	if len(files) == 0 {
		http.Error(w, "Pilih setidaknya satu file RAW untuk dikonversi.", http.StatusBadRequest)
		return
	}
	if len(files) > maxFiles {
		http.Error(w, "Maksimal 20 file RAW dapat dikonversi sekaligus.", http.StatusBadRequest)
		return
	}

	var totalInput int64
	for _, file := range files {
		filename := safeFilename(file.filename)
		if !rawExts[strings.ToLower(path.Ext(filename))] {
			http.Error(w, "Format RAW tidak didukung. Gunakan CR2, CR3, NEF, ARW, DNG, RAF, RW2, ORF, PEF, atau SRW.", http.StatusUnsupportedMediaType)
			return
		}
		totalInput += file.size
		if file.size <= 0 || totalInput > maxBytes {
			http.Error(w, "File kosong atau total ukuran file melebihi 200 MB.", http.StatusBadRequest)
			return
		}
	}

	var archive bytes.Buffer
	var zipWriter *zip.Writer
	if len(files) > 1 {
		zipWriter = zip.NewWriter(&archive)
	}
	usedNames := make(map[string]bool, len(files))
	var totalOutput int64
	for _, source := range files {
		file, err := source.open()
		if err != nil {
			http.Error(w, "File tidak dapat dibaca.", http.StatusBadRequest)
			return
		}
		input, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
		file.Close()
		if readErr != nil || len(input) == 0 || int64(len(input)) != source.size || len(input) > maxBytes {
			http.Error(w, "File kosong, tidak dapat dibaca, atau melebihi 200 MB.", http.StatusBadRequest)
			return
		}
		filename := safeFilename(source.filename)
		result, err := convertRAW(r.Context(), input, filename)
		if err != nil {
			fmt.Printf("Konversi %q gagal: %v\n", filename, err)
			http.Error(w, "Konversi gagal. Coba lagi beberapa saat.", http.StatusBadGateway)
			return
		}
		totalOutput += int64(len(result))
		if totalOutput > maxBytes {
			http.Error(w, "Total ukuran hasil JPG melebihi 200 MB.", http.StatusBadGateway)
			return
		}
		outputName := uniqueOutputName(strings.TrimSuffix(filename, path.Ext(filename))+".jpg", usedNames)
		if len(files) == 1 {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": outputName}))
			w.Header().Set("Content-Length", fmt.Sprint(len(result)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(result)
			return
		}
		entry, err := zipWriter.Create(outputName)
		if err != nil {
			http.Error(w, "Arsip ZIP tidak dapat dibuat.", http.StatusInternalServerError)
			return
		}
		if _, err := entry.Write(result); err != nil {
			http.Error(w, "Hasil JPG tidak dapat dimasukkan ke arsip ZIP.", http.StatusInternalServerError)
			return
		}
	}
	if err := zipWriter.Close(); err != nil {
		http.Error(w, "Arsip ZIP tidak dapat diselesaikan.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "raw-to-jpg.zip"}))
	w.Header().Set("Content-Length", fmt.Sprint(archive.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive.Bytes())
}

type uploadManager struct {
	mu       sync.RWMutex
	sessions map[string]*uploadSession
}

type uploadSession struct {
	mu      sync.Mutex
	dir     string
	files   []stagedFile
	updated time.Time
	closed  bool
}

type stagedFile struct {
	filename string
	size     int64
	received int64
	path     string
}

type uploadFileRequest struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type uploadStartRequest struct {
	Files []uploadFileRequest `json:"files"`
}

type uploadStartResponse struct {
	ID        string `json:"id"`
	ChunkSize int    `json:"chunkSize"`
}

func newUploadManager() *uploadManager {
	return &uploadManager{sessions: make(map[string]*uploadSession)}
}

func (m *uploadManager) startHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var request uploadStartRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Daftar file tidak valid.", http.StatusBadRequest)
		return
	}
	if len(request.Files) == 0 || len(request.Files) > maxFiles {
		http.Error(w, "Pilih 1 hingga 20 file RAW.", http.StatusBadRequest)
		return
	}

	session := &uploadSession{updated: time.Now(), files: make([]stagedFile, len(request.Files))}
	for i, file := range request.Files {
		filename := safeFilename(file.Name)
		if !rawExts[strings.ToLower(path.Ext(filename))] {
			http.Error(w, "Format RAW tidak didukung.", http.StatusUnsupportedMediaType)
			return
		}
		if file.Size <= 0 || file.Size > maxBytes {
			http.Error(w, "File kosong atau ukuran file melebihi batas 200 MB.", http.StatusBadRequest)
			return
		}
		session.files[i] = stagedFile{filename: filename, size: file.Size}
	}

	var totalSize int64
	for _, file := range session.files {
		if file.size > maxBytes-totalSize {
			http.Error(w, "Total ukuran file melebihi 200 MB.", http.StatusRequestEntityTooLarge)
			return
		}
		totalSize += file.size
	}
	id, err := newTaskID()
	if err != nil {
		http.Error(w, "Sesi upload tidak dapat dibuat.", http.StatusInternalServerError)
		return
	}
	session.dir, err = os.MkdirTemp("", "rawtojpg-upload-")
	if err != nil {
		http.Error(w, "Penyimpanan sementara tidak dapat dibuat.", http.StatusInternalServerError)
		return
	}
	for i := range session.files {
		session.files[i].path = filepath.Join(session.dir, strconv.Itoa(i))
	}
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(uploadStartResponse{ID: id, ChunkSize: uploadChunkSize}); err != nil {
		fmt.Printf("Respons awal upload tidak dapat dikirim: %v\n", err)
	}
}

func (m *uploadManager) chunkHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		http.Error(w, "Indeks file tidak valid.", http.StatusBadRequest)
		return
	}
	session := m.get(id)
	if session == nil {
		http.Error(w, "Sesi upload tidak ditemukan atau sudah kedaluwarsa.", http.StatusNotFound)
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || time.Since(session.updated) > uploadSessionTTL {
		http.Error(w, "Sesi upload sudah kedaluwarsa.", http.StatusGone)
		return
	}
	if index < 0 || index >= len(session.files) {
		http.Error(w, "Indeks file tidak valid.", http.StatusBadRequest)
		return
	}
	file := &session.files[index]
	offset, err := strconv.ParseInt(r.Header.Get("X-Chunk-Offset"), 10, 64)
	if err != nil || offset != file.received || offset >= file.size {
		http.Error(w, "Offset bagian upload tidak valid.", http.StatusConflict)
		return
	}
	expected := min(int64(uploadChunkSize), file.size-offset)
	r.Body = http.MaxBytesReader(w, r.Body, expected+1)
	data, err := io.ReadAll(r.Body)
	if err != nil || int64(len(data)) != expected {
		http.Error(w, "Ukuran bagian upload tidak valid.", http.StatusBadRequest)
		return
	}
	out, err := os.OpenFile(file.path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		http.Error(w, "File sementara tidak dapat ditulis.", http.StatusInternalServerError)
		return
	}
	written, writeErr := out.WriteAt(data, offset)
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil || written != len(data) {
		http.Error(w, "Bagian upload tidak dapat disimpan.", http.StatusInternalServerError)
		return
	}
	file.received += int64(written)
	session.updated = time.Now()
	w.WriteHeader(http.StatusNoContent)
}

func (m *uploadManager) convertHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session := m.get(id)
	if session == nil {
		http.Error(w, "Sesi upload tidak ditemukan atau sudah kedaluwarsa.", http.StatusNotFound)
		return
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		http.Error(w, "Sesi upload sudah ditutup.", http.StatusGone)
		return
	}
	for _, file := range session.files {
		if file.received != file.size {
			session.mu.Unlock()
			http.Error(w, "Upload belum lengkap.", http.StatusBadRequest)
			return
		}
	}
	session.closed = true
	files := append([]stagedFile(nil), session.files...)
	dir := session.dir
	session.mu.Unlock()
	m.remove(id, session)
	defer os.RemoveAll(dir)

	sources := make([]inputFile, 0, len(files))
	for _, file := range files {
		file := file
		sources = append(sources, inputFile{
			filename: file.filename,
			size:     file.size,
			open: func() (io.ReadCloser, error) {
				return os.Open(file.path)
			},
		})
	}
	convertFiles(w, r, sources)
}

func (m *uploadManager) cancelHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session := m.get(id)
	if session != nil {
		session.mu.Lock()
		if session.closed {
			session.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		session.closed = true
		dir := session.dir
		session.mu.Unlock()
		m.remove(id, session)
		_ = os.RemoveAll(dir)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *uploadManager) get(id string) *uploadSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

func (m *uploadManager) remove(id string, session *uploadSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[id] == session {
		delete(m.sessions, id)
	}
}

func (m *uploadManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.RLock()
		sessions := make(map[string]*uploadSession, len(m.sessions))
		for id, session := range m.sessions {
			sessions[id] = session
		}
		m.mu.RUnlock()
		for id, session := range sessions {
			session.mu.Lock()
			expired := !session.closed && time.Since(session.updated) > uploadSessionTTL
			if expired {
				session.closed = true
			}
			dir := session.dir
			session.mu.Unlock()
			if expired {
				m.remove(id, session)
				_ = os.RemoveAll(dir)
			}
		}
	}
}

func uniqueOutputName(filename string, used map[string]bool) string {
	if !used[filename] {
		used[filename] = true
		return filename
	}
	extension := path.Ext(filename)
	base := strings.TrimSuffix(filename, extension)
	for count := 2; ; count++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, count, extension)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
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
