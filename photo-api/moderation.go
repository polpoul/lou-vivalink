package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Config (activé seulement si MODERATION_ENABLED=true) ──────────────────────

var (
	moderationEnabled  = os.Getenv("MODERATION_ENABLED") == "true"
	statusPath         = getEnv("STATUS_PATH", "/opt/auth/status-ca.json")
	uploaderMetaPath   = getEnv("UPLOADER_META_PATH", "/opt/auth/uploader-meta-ca.json")
	uploaderTokensPath = getEnv("UPLOADER_TOKENS_PATH", "/opt/auth/uploader-tokens-ca.json")
	moderationService  = getEnv("MODERATION_SERVICE", "ca")
	uploaderQuota      = func() int {
		n, _ := strconv.Atoi(getEnv("UPLOADER_QUOTA", "100"))
		if n <= 0 {
			return 100
		}
		return n
	}()
	modSmtpHost = getEnv("SMTP_HOST", "")
	modSmtpPort = getEnv("SMTP_PORT", "587")
	modSmtpUser = getEnv("SMTP_USER", "")
	modSmtpPass = getEnv("SMTP_PASS", "")
	modSmtpFrom = getEnv("SMTP_FROM", "")
	appURL      = getEnv("APP_URL", "https://ca.vivalink.top")
)

// ── Types de données ──────────────────────────────────────────────────────────

// StatusStore : filename → "pending" | "approved" | "refused"
type StatusStore map[string]string

// UploaderMeta : filename → email de l'uploader
type UploaderMeta map[string]string

// UploaderToken : entrée dans le store des tokens uploader
type UploaderToken struct {
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

// UploaderTokenStore : sha256(token) → UploaderToken
type UploaderTokenStore map[string]UploaderToken

// ── Helpers fichiers JSON ─────────────────────────────────────────────────────

func loadStatusStore() (StatusStore, error) {
	data, err := os.ReadFile(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusStore{}, nil
		}
		return nil, err
	}
	var s StatusStore
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return s, nil
}

func saveStatusStore(s StatusStore) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statusPath, data, 0644)
}

func loadUploaderMeta() (UploaderMeta, error) {
	data, err := os.ReadFile(uploaderMetaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return UploaderMeta{}, nil
		}
		return nil, err
	}
	var m UploaderMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func saveUploaderMeta(m UploaderMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(uploaderMetaPath, data, 0644)
}

func loadUploaderTokenStore() (UploaderTokenStore, error) {
	data, err := os.ReadFile(uploaderTokensPath)
	if err != nil {
		if os.IsNotExist(err) {
			return UploaderTokenStore{}, nil
		}
		return nil, err
	}
	var s UploaderTokenStore
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return s, nil
}

func saveUploaderTokenStore(s UploaderTokenStore) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(uploaderTokensPath, data, 0644)
}

// ── Helpers tokens ────────────────────────────────────────────────────────────

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// emailToID8 : dérive un ID de 8 chars depuis un email pour le nom de fichier
func emailToID8(email string) string {
	h := sha256.Sum256([]byte(strings.ToLower(email)))
	return hex.EncodeToString(h[:])[:8]
}

// ── Auth uploader ─────────────────────────────────────────────────────────────

// getUploaderEmailFromToken extrait l'email uploader depuis le header Authorization: Bearer
func getUploaderEmailFromToken(r *http.Request) (string, bool) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" {
		return "", false
	}
	store, err := loadUploaderTokenStore()
	if err != nil {
		return "", false
	}
	entry, ok := store[hashToken(token)]
	if !ok {
		return "", false
	}
	return entry.Email, true
}

// isModerator vérifie : token auth-service valide + email dans allowlist avec le service de modération
func isModerator(r *http.Request) bool {
	if !verifyToken(r) {
		return false
	}
	email := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Moderator-Email")))
	if email == "" {
		return false
	}
	al, err := loadAllowlist()
	if err != nil {
		return false
	}
	for _, svc := range al[email] {
		if svc == moderationService {
			return true
		}
	}
	return false
}

func moderatorRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isModerator(r) {
			jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "moderator access required"})
			return
		}
		next(w, r)
	}
}

func uploaderRequired(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := getUploaderEmailFromToken(r); !ok {
			jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "uploader token required"})
			return
		}
		next(w, r)
	}
}

// ── Quota ─────────────────────────────────────────────────────────────────────

func countUploaderPhotos(email string, meta UploaderMeta) int {
	count := 0
	for _, e := range meta {
		if e == email {
			count++
		}
	}
	return count
}

// ── Email ─────────────────────────────────────────────────────────────────────

func sendUploaderMagicLink(email, token string) error {
	if modSmtpHost == "" || modSmtpUser == "" {
		return fmt.Errorf("SMTP non configuré")
	}
	link := fmt.Sprintf("%s/mes-photos.html?token=%s", appURL, token)
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: Vos photos - lien d'accès\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n"+
			"Bonjour,\r\n\r\n"+
			"Cliquez sur ce lien pour accéder à vos photos et en ajouter de nouvelles :\r\n\r\n"+
			"%s\r\n\r\n"+
			"Ce lien est personnel et permanent — ne le partagez pas.\r\n",
		modSmtpFrom, email, link,
	)
	auth := smtp.PlainAuth("", modSmtpUser, modSmtpPass, modSmtpHost)
	return smtp.SendMail(modSmtpHost+":"+modSmtpPort, auth, modSmtpFrom, []string{email}, []byte(msg))
}

// ── Handlers galerie publique ─────────────────────────────────────────────────

// GET /api/gallery/list — public, photos approuvées seulement
func handleGalleryList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ss, _ := loadStatusStore()
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
		if ext != ".jpg" && ext != ".jpeg" {
			continue
		}
		if ss[e.Name()] != "approved" {
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
		})
	}
	sort.Slice(photos, func(i, j int) bool {
		return photos[i].CreatedAt > photos[j].CreatedAt
	})
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

// ── Handler liste modérateur ──────────────────────────────────────────────────

// GET /api/photos/list — modérateur, toutes les photos avec statut
func handleModerationPhotoList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entries, err := os.ReadDir(photosDir)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot read photos dir"})
		return
	}
	ss, _ := loadStatusStore()
	meta, _ := loadUploaderMeta()
	ts, _ := loadTagStore()

	var photos []PhotoMeta
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".jpg" && ext != ".jpeg" {
			continue
		}
		info, _ := e.Info()
		var size int64
		var modTime time.Time
		if info != nil {
			size = info.Size()
			modTime = info.ModTime()
		}
		status := ss[e.Name()]
		if status == "" {
			status = "pending"
		}
		uploader := meta[e.Name()]
		if uploader == "" {
			uploader = extractUploader(e.Name())
		}
		p := PhotoMeta{
			Filename:  e.Name(),
			URL:       "/photos/" + e.Name(),
			Size:      size,
			CreatedAt: modTime.Format(time.RFC3339),
			Uploader:  uploader,
			Status:    status,
		}
		if ts != nil {
			if tags, ok := ts.Photos[e.Name()]; ok {
				p.Tags = tags
			} else {
				p.Tags = []string{}
			}
		}
		photos = append(photos, p)
	}
	sort.Slice(photos, func(i, j int) bool {
		return photos[i].CreatedAt > photos[j].CreatedAt
	})
	jsonResponse(w, http.StatusOK, photos)
}

// ── Handler upload uploader ───────────────────────────────────────────────────

// POST /api/photos/upload — token uploader requis, vérifie quota, statut pending
func handleUploaderUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	email, ok := getUploaderEmailFromToken(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	meta, err := loadUploaderMeta()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if countUploaderPhotos(email, meta) >= uploaderQuota {
		jsonResponse(w, http.StatusForbidden, map[string]string{"error": "quota atteint"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFileSize)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "fichier trop volumineux ou invalide"})
		return
	}
	files := r.MultipartForm.File["photos"]
	if len(files) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "aucun fichier"})
		return
	}
	ss, _ := loadStatusStore()
	userID := emailToID8(email)
	var uploaded []string

	for _, fh := range files {
		if countUploaderPhotos(email, meta) >= uploaderQuota {
			break
		}
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
		ss[filename] = "pending"
		meta[filename] = email
		uploaded = append(uploaded, filename)
	}

	if len(uploaded) > 0 {
		if err := saveStatusStore(ss); err != nil {
			log.Printf("saveStatusStore error: %v", err)
		}
		if err := saveUploaderMeta(meta); err != nil {
			log.Printf("saveUploaderMeta error: %v", err)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"uploaded": uploaded,
		"count":    len(uploaded),
	})
}

// ── Handler suppression modérée ───────────────────────────────────────────────

// DELETE /api/photos/{filename} — modérateur OU uploader propriétaire
func handleDeleteModerated(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}
	isMod := isModerator(r)
	uploaderEmail, isUploader := getUploaderEmailFromToken(r)
	if !isMod && !isUploader {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	meta, _ := loadUploaderMeta()
	if isUploader && !isMod {
		if meta[filename] != uploaderEmail {
			jsonResponse(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
	}
	path := filepath.Join(photosDir, filename)
	if err := os.Remove(path); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	// Nettoyage métadonnées
	ss, _ := loadStatusStore()
	delete(ss, filename)
	saveStatusStore(ss)
	delete(meta, filename)
	saveUploaderMeta(meta)
	ts, _ := loadTagStore()
	if ts != nil {
		delete(ts.Photos, filename)
		saveTagStore(ts)
	}
	jsonResponse(w, http.StatusOK, map[string]string{"deleted": filename})
}

// ── Handlers approbation / refus ──────────────────────────────────────────────

func handleApprove(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	filename := strings.TrimSuffix(trimmed, "/approve")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}
	if _, err := os.Stat(filepath.Join(photosDir, filename)); os.IsNotExist(err) {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	ss, _ := loadStatusStore()
	ss[filename] = "approved"
	if err := saveStatusStore(ss); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save status"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"filename": filename, "status": "approved"})
}

func handleRefuse(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	filename := strings.TrimSuffix(trimmed, "/refuse")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}
	if _, err := os.Stat(filepath.Join(photosDir, filename)); os.IsNotExist(err) {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	ss, _ := loadStatusStore()
	ss[filename] = "refused"
	if err := saveStatusStore(ss); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "cannot save status"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"filename": filename, "status": "refused"})
}

// ── Router photos modéré ──────────────────────────────────────────────────────

func photosRouterModerated(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodDelete:
		handleDeleteModerated(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/approve"):
		moderatorRequired(handleApprove)(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/refuse"):
		moderatorRequired(handleRefuse)(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/date"):
		moderatorRequired(handlePatchDate)(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/rotate"):
		moderatorRequired(handleRotate)(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/tags"):
		moderatorRequired(handlePatchTags)(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ── Handlers auth uploader ────────────────────────────────────────────────────

// POST /api/uploader/request-login — génère et envoie un magic link uploader
func handleUploaderRequestLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" || !strings.Contains(email, "@") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "email invalide"})
		return
	}
	token, err := generateToken()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	store, err := loadUploaderTokenStore()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	// Supprimer les anciens tokens de cet email
	for k, v := range store {
		if v.Email == email {
			delete(store, k)
		}
	}
	store[hashToken(token)] = UploaderToken{Email: email, CreatedAt: time.Now().Format(time.RFC3339)}
	if err := saveUploaderTokenStore(store); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := sendUploaderMagicLink(email, token); err != nil {
		log.Printf("sendUploaderMagicLink error for %s: %v", email, err)
	}
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GET /api/uploader/login?token= — échange le token contre une session
func handleUploaderLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "token manquant"})
		return
	}
	store, err := loadUploaderTokenStore()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	entry, ok := store[hashToken(token)]
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "token invalide"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{
		"uploader_token": token,
		"email":          entry.Email,
	})
}

// GET /api/uploader/me — infos uploader (email + quota)
func handleUploaderMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	email, ok := getUploaderEmailFromToken(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	meta, _ := loadUploaderMeta()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"email": email,
		"quota": uploaderQuota,
		"used":  countUploaderPhotos(email, meta),
	})
}

// GET /api/uploader/photos — photos de l'uploader avec statut
func handleUploaderPhotos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	email, ok := getUploaderEmailFromToken(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	meta, _ := loadUploaderMeta()
	ss, _ := loadStatusStore()
	ts, _ := loadTagStore()
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
		if ext != ".jpg" && ext != ".jpeg" {
			continue
		}
		if meta[e.Name()] != email {
			continue
		}
		info, _ := e.Info()
		var size int64
		var modTime time.Time
		if info != nil {
			size = info.Size()
			modTime = info.ModTime()
		}
		status := ss[e.Name()]
		if status == "" {
			status = "pending"
		}
		p := PhotoMeta{
			Filename:  e.Name(),
			URL:       "/photos/" + e.Name(),
			Size:      size,
			CreatedAt: modTime.Format(time.RFC3339),
			Uploader:  email,
			Status:    status,
		}
		if ts != nil {
			if tags, ok := ts.Photos[e.Name()]; ok {
				p.Tags = tags
			} else {
				p.Tags = []string{}
			}
		}
		photos = append(photos, p)
	}
	sort.Slice(photos, func(i, j int) bool {
		return photos[i].CreatedAt > photos[j].CreatedAt
	})
	jsonResponse(w, http.StatusOK, photos)
}

// ── Enregistrement des routes ─────────────────────────────────────────────────

func registerModerationRoutes(mux *http.ServeMux) {
	// Auth uploader
	mux.HandleFunc("/api/uploader/request-login", handleUploaderRequestLogin)
	mux.HandleFunc("/api/uploader/login", handleUploaderLogin)
	mux.HandleFunc("/api/uploader/me", uploaderRequired(handleUploaderMe))
	mux.HandleFunc("/api/uploader/photos", uploaderRequired(handleUploaderPhotos))

	// Upload (auth uploader, pas auth-service)
	mux.HandleFunc("/api/photos/upload", handleUploaderUpload)

	// Galerie publique (approuvées seulement)
	mux.HandleFunc("/api/gallery/list", handleGalleryList)

	// Liste modérateur + actions par photo
	mux.HandleFunc("/api/photos/list", moderatorRequired(handleModerationPhotoList))
	mux.HandleFunc("/api/photos/", photosRouterModerated)

	// Tags publics (filtre galerie)
	mux.HandleFunc("/api/tags", handleGetTags)
}
