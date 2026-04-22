package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/gif"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	photosDir    = "/opt/photos"
	maxDimension = 1920
	maxFileSize  = 50 << 20 // 50MB
)

var authServiceURL = getEnv("AUTH_SERVICE_URL", "http://auth-service:8080")

type PhotoMeta struct {
	Filename  string   `json:"filename"`
	URL       string   `json:"url"`
	Size      int64    `json:"size"`
	CreatedAt string   `json:"created_at"`
	Uploader  string   `json:"uploader"`
	Tags      []string `json:"tags"`
}

// extractUploader extrait les 8 chars du user_id depuis le nom de fichier
// Format attendu : timestamp_userid8chars_nom.jpg
func extractUploader(filename string) string {
	parts := strings.SplitN(filename, "_", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return "unknown"
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := []string{
			"https://lou.vivalink.top",
			"https://voyage.vivalink.top",
			"http://localhost:3000",
		}
		for _, o := range allowed {
			if origin == o {
				w.Header().Set("Access-Control-Allow-Origin", o)
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Admin-Email")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func verifyToken(r *http.Request) bool {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return false
	}
	req, err := http.NewRequest("GET", authServiceURL+"/auth/me", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", authHeader)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func getUserID(r *http.Request) string {
	authHeader := r.Header.Get("Authorization")
	req, err := http.NewRequest("GET", authServiceURL+"/auth/me", nil)
	if err != nil {
		return "unknown"
	}
	req.Header.Set("Authorization", authHeader)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()
	var data struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "unknown"
	}
	if data.UserID == "" {
		return "unknown"
	}
	if len(data.UserID) > 8 {
		return data.UserID[:8]
	}
	return data.UserID
}

func authRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifyToken(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func resizeImage(src image.Image) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= maxDimension && h <= maxDimension {
		return src
	}
	ratio := float64(maxDimension) / float64(w)
	if float64(h)*ratio > float64(maxDimension) {
		ratio = float64(maxDimension) / float64(h)
	}
	newW := int(float64(w) * ratio)
	newH := int(float64(h) * ratio)
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "file too large or invalid"})
		return
	}
	files := r.MultipartForm.File["photos"]
	if len(files) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "no files provided"})
		return
	}

	userID := getUserID(r)

	var uploaded []string
	for _, fh := range files {
		file, err := fh.Open()
		if err != nil {
			continue
		}
		defer file.Close()
		ext := strings.ToLower(filepath.Ext(fh.Filename))
		if ext == "" {
			ext = ".jpg"
		}
		var img image.Image
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			img, _, err = image.Decode(file)
		default:
			continue
		}
		if err != nil {
			log.Printf("decode error for %s: %v", fh.Filename, err)
			continue
		}
		img = resizeImage(img)
		timestamp := time.Now().UnixNano()
		baseName := strings.TrimSuffix(fh.Filename, filepath.Ext(fh.Filename))
		baseName = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '-'
		}, baseName)
		filename := fmt.Sprintf("%d_%s_%s.jpg", timestamp, userID, baseName)
		outPath := filepath.Join(photosDir, filename)
		out, err := os.Create(outPath)
		if err != nil {
			log.Printf("create file error: %v", err)
			continue
		}
		if err := jpeg.Encode(out, img, &jpeg.Options{Quality: 88}); err != nil {
			out.Close()
			os.Remove(outPath)
			continue
		}
		out.Close()
		uploaded = append(uploaded, filename)
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"uploaded": uploaded,
		"count":    len(uploaded),
	})
}

func handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries, err := os.ReadDir(photosDir)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot read photos dir"})
		return
	}
	var photos []PhotoMeta
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".gif" && ext != ".webp" {
			continue
		}
		info, _ := e.Info()
		var size int64
		var modTime time.Time
		if info != nil {
			size = info.Size()
			modTime = info.ModTime()
		}
		photos = append(photos, PhotoMeta{
			Filename:  e.Name(),
			URL:       "/photos/" + e.Name(),
			Size:      size,
			CreatedAt: modTime.Format(time.RFC3339),
			Uploader:  extractUploader(e.Name()),
		})
	}
	sort.Slice(photos, func(i, j int) bool {
		return photos[i].CreatedAt > photos[j].CreatedAt
	})
	// Résoudre les pseudos depuis members.json
	members, _ := loadMembers()
	if len(members) > 0 {
		for i := range photos {
			if pseudo, ok := members[photos[i].Uploader]; ok {
				photos[i].Uploader = pseudo
			}
		}
	}
	// Injecter les tags depuis le TagStore
	ts, _ := loadTagStore()
	if ts != nil {
		for i := range photos {
			if tags, ok := ts.Photos[photos[i].Filename]; ok {
				photos[i].Tags = tags
			} else {
				photos[i].Tags = []string{}
			}
		}
	}
	jsonResponse(w, http.StatusOK, photos)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}
	path := filepath.Join(photosDir, filename)
	if err := os.Remove(path); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"deleted": filename})
}

// PATCH /api/photos/{filename}/date
func handlePatchDate(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	filename := strings.TrimSuffix(trimmed, "/date")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}
	var body struct {
		Date string `json:"date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	t, err := time.Parse("2006-01-02", body.Date)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid date, use YYYY-MM-DD"})
		return
	}
	filePath := filepath.Join(photosDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if err := os.Chtimes(filePath, t, t); err != nil {
		log.Printf("Chtimes error: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "failed to update date"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{
		"filename": filename,
		"date":     t.Format(time.RFC3339),
	})
}

// PATCH /api/photos/{filename}/rotate — pivote l'image de 90° dans le sens horaire
func handleRotate(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	filename := strings.TrimSuffix(trimmed, "/rotate")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}

	filePath := filepath.Join(photosDir, filename)
	f, err := os.Open(filePath)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot decode image"})
		return
	}

	// Rotation 90° horaire
	bounds := img.Bounds()
	newW := bounds.Dy()
	newH := bounds.Dx()
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			dst.Set(newW-1-(y-bounds.Min.Y), x-bounds.Min.X, img.At(x, y))
		}
	}

	out, err := os.Create(filePath)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save image"})
		return
	}
	defer out.Close()
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: 88}); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot encode image"})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"rotated": filename})
}

// Router pour /api/photos/{filename}
func photosRouter(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodDelete:
		handleDelete(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/date"):
		handlePatchDate(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/rotate"):
		handleRotate(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/tags"):
		handlePatchTags(w, r)
	default:
		http.NotFound(w, r)
	}
}

func handleServePhoto(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/photos/")
	if filename == "" || strings.Contains(filename, "..") {
		http.NotFound(w, r)
		return
	}
	filePath := filepath.Join(photosDir, filename)
	http.ServeFile(w, r, filePath)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "ok")
}

// ── Admin ─────────────────────────────────────────────────────────────────────

var (
	allowlistPath = getEnv("ALLOWLIST_PATH", "/opt/auth/allowlist.json")
	membersPath   = getEnv("MEMBERS_PATH", "/opt/auth/members.json")
	tagsPath      = getEnv("TAGS_PATH", "")
	adminEmail    = getEnv("ADMIN_EMAIL", "deleupa@gmail.com")
)

type Allowlist map[string][]string
type Members map[string]string

// TagStore contient la liste des tags disponibles et les affectations par photo
type TagStore struct {
	Tags   []string            `json:"tags"`
	Photos map[string][]string `json:"photos"`
}

func loadTagStore() (*TagStore, error) {
	if tagsPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(tagsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &TagStore{Tags: []string{}, Photos: map[string][]string{}}, nil
		}
		return nil, err
	}
	var ts TagStore
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, err
	}
	if ts.Photos == nil {
		ts.Photos = map[string][]string{}
	}
	return &ts, nil
}

func saveTagStore(ts *TagStore) error {
	if tagsPath == "" {
		return nil
	}
	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tagsPath, data, 0644)
}

func loadAllowlist() (Allowlist, error) {
	data, err := os.ReadFile(allowlistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Allowlist{}, nil
		}
		return nil, err
	}
	var al Allowlist
	if err := json.Unmarshal(data, &al); err != nil {
		return nil, err
	}
	return al, nil
}

func saveAllowlist(al Allowlist) error {
	data, err := json.MarshalIndent(al, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(allowlistPath, data, 0644)
}

func loadMembers() (Members, error) {
	data, err := os.ReadFile(membersPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Members{}, nil
		}
		return nil, err
	}
	var m Members
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func saveMembers(m Members) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(membersPath, data, 0644)
}

func adminRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !verifyToken(r) {
			jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		callerEmail := r.Header.Get("X-Admin-Email")
		if strings.ToLower(callerEmail) != strings.ToLower(adminEmail) {
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		next(w, r)
	}
}

func handleAdminInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Email       string   `json:"email"`
		Services    []string `json:"services"`
		RedirectURL string   `json:"redirect_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" || !strings.Contains(email, "@") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid email"})
		return
	}
	if len(body.Services) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "no services specified"})
		return
	}
	al, err := loadAllowlist()
	if err != nil {
		log.Printf("loadAllowlist error: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot load allowlist"})
		return
	}
	existing := al[email]
	for _, svc := range body.Services {
		found := false
		for _, e := range existing {
			if e == svc {
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, svc)
		}
	}
	al[email] = existing
	if err := saveAllowlist(al); err != nil {
		log.Printf("saveAllowlist error: %v", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save allowlist"})
		return
	}

	// Envoyer le magic link sans champ "app" pour bypasser la vérification allowlist
	redirectURL := body.RedirectURL
	if redirectURL == "" && len(body.Services) > 0 {
		redirectURL = "https://" + body.Services[0] + ".vivalink.top"
	}
	loginReqBody, _ := json.Marshal(map[string]interface{}{
		"email":        email,
		"redirect_url": redirectURL,
	})
	authReq, err := http.NewRequest("POST", authServiceURL+"/auth/request-login",
		strings.NewReader(string(loginReqBody)))
	if err == nil {
		authReq.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 10 * time.Second}
		authResp, err := client.Do(authReq)
		if err != nil {
			log.Printf("request-login error: %v", err)
		} else {
			authResp.Body.Close()
		}
	}

	log.Printf("admin: invited %s for services %v", email, body.Services)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"invited":  email,
		"services": existing,
	})
}

func handleAdminAllowlist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	al, err := loadAllowlist()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot load allowlist"})
		return
	}
	jsonResponse(w, http.StatusOK, al)
}

func handleAdminRemoveMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Email   string `json:"email"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	al, err := loadAllowlist()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot load allowlist"})
		return
	}
	if body.Service == "" {
		delete(al, email)
	} else {
		svcs := al[email]
		newSvcs := svcs[:0]
		for _, s := range svcs {
			if s != body.Service {
				newSvcs = append(newSvcs, s)
			}
		}
		if len(newSvcs) == 0 {
			delete(al, email)
		} else {
			al[email] = newSvcs
		}
	}
	if err := saveAllowlist(al); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save allowlist"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"removed": email})
}

func handleAdminGetMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m, err := loadMembers()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot load members"})
		return
	}
	jsonResponse(w, http.StatusOK, m)
}

func handleAdminSaveMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var m Members
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := saveMembers(m); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save members"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"saved": "ok"})
}

// PATCH /api/photos/{filename}/tags — body: { "tags": ["Plage", "Randonnée"] }
func handlePatchTags(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	filename := strings.TrimSuffix(trimmed, "/tags")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	ts, err := loadTagStore()
	if err != nil || ts == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "tags not configured"})
		return
	}
	if body.Tags == nil || len(body.Tags) == 0 {
		delete(ts.Photos, filename)
	} else {
		ts.Photos[filename] = body.Tags
	}
	if err := saveTagStore(ts); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save tags"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{"filename": filename, "tags": body.Tags})
}

// GET /api/tags — retourne la liste des tags disponibles
func handleGetTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ts, err := loadTagStore()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot load tags"})
		return
	}
	if ts == nil {
		jsonResponse(w, http.StatusOK, []string{})
		return
	}
	jsonResponse(w, http.StatusOK, ts.Tags)
}

// POST /api/admin/tags — admin : met à jour la liste des tags disponibles
func handleAdminSaveTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var tags []string
	if err := json.NewDecoder(r.Body).Decode(&tags); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	ts, err := loadTagStore()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot load tags"})
		return
	}
	if ts == nil {
		ts = &TagStore{Photos: map[string][]string{}}
	}
	ts.Tags = tags
	if err := saveTagStore(ts); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save tags"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"saved": "ok"})
}

func main() {
	if err := os.MkdirAll(photosDir, 0755); err != nil {
		log.Fatalf("cannot create photos dir: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/photos/upload", authRequired(handleUpload))
	mux.HandleFunc("/api/photos/list", authRequired(handleList))
	mux.HandleFunc("/api/gallery/list", authRequired(handleList))
	mux.HandleFunc("/api/photos/", authRequired(photosRouter))
	mux.HandleFunc("/photos/", handleServePhoto)

	// Routes admin
	mux.HandleFunc("/api/admin/invite", adminRequired(handleAdminInvite))
	mux.HandleFunc("/api/admin/allowlist", adminRequired(handleAdminAllowlist))
	mux.HandleFunc("/api/admin/member", adminRequired(handleAdminRemoveMember))
	mux.HandleFunc("/api/admin/members", adminRequired(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			handleAdminGetMembers(w, r)
		} else if r.Method == http.MethodPost {
			handleAdminSaveMembers(w, r)
		} else {
			http.NotFound(w, r)
		}
	}))

	// Routes tags
	mux.HandleFunc("/api/tags", authRequired(handleGetTags))
	mux.HandleFunc("/api/admin/tags", adminRequired(handleAdminSaveTags))

	handler := corsMiddleware(mux)
	log.Println("photo-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", handler))
}
