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
	Filename  string `json:"filename"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
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

// getUserID appelle /auth/me et retourne les 8 premiers caractères du user_id
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
	// Garder les 8 premiers caractères de l'UUID
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

	// Récupérer le user_id une seule fois pour tous les fichiers de cet upload
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
		// Nom du fichier : timestamp_userID_nomoriginal.jpg
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
		})
	}
	sort.Slice(photos, func(i, j int) bool {
		return photos[i].CreatedAt > photos[j].CreatedAt
	})
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

// PATCH /api/photos/{filename}/date — body: { "date": "2024-06-15" }
func handlePatchDate(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/photos/")
	filename := strings.TrimSuffix(trimmed, "/date")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
		return
	}

	var body struct {
		Date string `json:"date"` // "2006-01-02"
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

// Router pour /api/photos/{filename} — dispatche DELETE et PATCH /date
func photosRouter(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodDelete:
		handleDelete(w, r)
	case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/date"):
		handlePatchDate(w, r)
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

	handler := corsMiddleware(mux)
	log.Println("photo-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", handler))
}
