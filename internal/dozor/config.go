package dozor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}
type Camera struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	SubURL      string  `json:"sub_url"`
	Username    string  `json:"username"`
	Password    string  `json:"password,omitempty"`
	ONVIF       string  `json:"onvif"`
	Motion      string  `json:"motion"`
	Sensitivity float64 `json:"sensitivity"`
	Masks       []Rect  `json:"masks"`
	Enabled     bool    `json:"enabled"`
}
type S3Config struct {
	Enabled        bool   `json:"enabled"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	AccessKey      string `json:"access_key,omitempty"`
	SecretKey      string `json:"secret_key,omitempty"`
	PathStyle      bool   `json:"path_style"`
	BytesPerSecond int64  `json:"bytes_per_second"`
}
type Config struct {
	Listen       string   `json:"listen"`
	Archive      string   `json:"archive"`
	DiskUUID     string   `json:"disk_uuid"`
	Timezone     string   `json:"timezone"`
	Cameras      []Camera `json:"cameras"`
	S3           S3Config `json:"s3"`
	AutoUpdate   bool     `json:"auto_update"`
	ReleaseURL   string   `json:"release_url"`
	PasswordHash string   `json:"password_hash,omitempty"`
	TLSCert      string   `json:"tls_cert,omitempty"`
	TLSKey       string   `json:"tls_key,omitempty"`
}
type ConfigFile struct {
	mu    sync.RWMutex
	Path  string
	value Config
}

func ID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

var safeID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func WriteJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return AtomicWrite(path, append(b, '\n'), 0600)
}
func LoadConfig(path string) (*ConfigFile, error) {
	c := Config{Listen: "127.0.0.1:8080", Archive: "/srv/dozor", Timezone: "Europe/Moscow", Cameras: []Camera{}, S3: S3Config{Region: "us-east-1", Prefix: "dozor", BytesPerSecond: 2 * 1024 * 1024}, AutoUpdate: true, ReleaseURL: DefaultReleaseURL}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = WriteJSON(path, c)
	} else if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.ReleaseURL) == "" {
		c.ReleaseURL = DefaultReleaseURL
	}
	if err = ValidateConfig(c); err != nil {
		return nil, err
	}
	return &ConfigFile{Path: path, value: c}, nil
}
func (f *ConfigFile) Get() Config {
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, _ := json.Marshal(f.value)
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}
func (f *ConfigFile) Save(c Config) error {
	if strings.TrimSpace(c.ReleaseURL) == "" {
		c.ReleaseURL = DefaultReleaseURL
	}
	if e := ValidateConfig(c); e != nil {
		return e
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := WriteJSON(f.Path, c); e != nil {
		return e
	}
	f.value = c
	return nil
}
func ValidateConfig(c Config) error {
	if !filepath.IsAbs(c.Archive) || filepath.Clean(c.Archive) == "/" {
		return errors.New("нужен абсолютный каталог архива")
	}
	if _, e := time.LoadLocation(c.Timezone); e != nil {
		return errors.New("неверный часовой пояс")
	}
	if c.S3.BytesPerSecond < 0 {
		return errors.New("лимит скорости не может быть отрицательным")
	}
	if c.S3.Enabled && (c.S3.Bucket == "" || c.S3.Region == "" || c.S3.AccessKey == "" || c.S3.SecretKey == "") {
		return errors.New("заполните настройки S3")
	}
	if c.S3.Endpoint != "" {
		u, e := url.Parse(c.S3.Endpoint)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("неверный endpoint S3")
		}
	}
	seen := map[string]bool{}
	for _, cam := range c.Cameras {
		if !safeID.MatchString(cam.ID) || seen[cam.ID] {
			return errors.New("неверный ID камеры")
		}
		seen[cam.ID] = true
		if e := ValidateCamera(cam); e != nil {
			return e
		}
	}
	return nil
}
func ValidateCamera(c Camera) error {
	if c.Name == "" || len(c.Name) > 100 {
		return errors.New("задайте название камеры до 100 символов")
	}
	for _, s := range []string{c.URL, c.SubURL} {
		if s == "" {
			continue
		}
		u, e := url.Parse(s)
		if e != nil || u.Scheme != "rtsp" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return errors.New("нужен RTSP URL без пароля; учётные данные вводятся отдельно")
		}
	}
	if c.URL == "" {
		return errors.New("укажите RTSP URL")
	}
	if c.Motion != "local" && c.Motion != "onvif" && c.Motion != "auto" {
		return errors.New("неверный источник движения")
	}
	if c.Motion == "local" && c.SubURL == "" {
		return errors.New("для локального детектора нужен дополнительный RTSP-поток")
	}
	if c.Motion == "auto" && c.SubURL == "" {
		return errors.New("для автоматического режима нужен дополнительный поток на случай недоступности ONVIF")
	}
	if c.Motion == "onvif" && c.ONVIF == "" {
		return errors.New("укажите ONVIF endpoint")
	}
	if c.ONVIF != "" {
		u, e := url.Parse(c.ONVIF)
		if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("неверный ONVIF endpoint")
		}
	}
	if c.Sensitivity <= 0 || c.Sensitivity > 1 {
		return errors.New("чувствительность должна быть от 0 до 1")
	}
	if len(c.Masks) > 32 {
		return errors.New("слишком много масок")
	}
	for _, r := range c.Masks {
		if r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > 1 || r.Y+r.H > 1 {
			return errors.New("маски задаются долями кадра от 0 до 1")
		}
	}
	return nil
}
func CameraURL(c Camera, sub bool) string {
	s := c.URL
	if sub {
		s = c.SubURL
	}
	u, e := url.Parse(s)
	if e != nil {
		return ""
	}
	if c.Username != "" {
		u.User = url.UserPassword(c.Username, c.Password)
	}
	return u.String()
}
func PublicConfig(c Config) map[string]any {
	hasS3 := c.S3.SecretKey != ""
	c.PasswordHash = ""
	c.S3.SecretKey = ""
	c.S3.AccessKey = ""
	c.TLSKey = ""
	cams := make([]map[string]any, 0, len(c.Cameras))
	for _, cam := range c.Cameras {
		has := cam.Password != ""
		cam.Password = ""
		b, _ := json.Marshal(cam)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["has_password"] = has
		cams = append(cams, m)
	}
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["cameras"] = cams
	m["has_s3_credentials"] = hasS3
	return m
}
func RestoreSecrets(next *Config, old Config) {
	next.Listen = old.Listen
	next.Archive = old.Archive
	next.DiskUUID = old.DiskUUID
	next.PasswordHash = old.PasswordHash
	next.TLSCert = old.TLSCert
	next.TLSKey = old.TLSKey
	if next.S3.AccessKey == "" {
		next.S3.AccessKey = old.S3.AccessKey
	}
	if next.S3.SecretKey == "" {
		next.S3.SecretKey = old.S3.SecretKey
	}
	for i := range next.Cameras {
		for _, prev := range old.Cameras {
			if prev.ID == next.Cameras[i].ID && next.Cameras[i].Password == "" {
				next.Cameras[i].Password = prev.Password
			}
		}
	}
}
func checkedPath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) || rel == "." || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("unsafe path")
	}
	return filepath.Join(root, rel), nil
}
