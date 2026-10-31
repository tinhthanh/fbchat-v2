package main

// Cookie phiên và cursor của db 205 nằm trong FBLS_DATA (volume riêng, quyền 600).
// Ghi kiểu tạm rồi đổi tên: tiến trình bị dừng giữa chừng không làm hỏng file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// CookieJar: cookie facebook.com dạng {"tên": "giá trị"}. Không ghi giá trị ra log.
type CookieJar struct {
	path string
	mu   sync.Mutex
	m    map[string]string
}

var requiredCookies = []string{"c_user", "xs"}

func OpenCookieJar(path string) (*CookieJar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cookies: %w", err)
	}
	jar := &CookieJar{path: path, m: map[string]string{}}
	if err := json.Unmarshal(data, &jar.m); err != nil {
		return nil, fmt.Errorf("cookies file must be a JSON object name -> value: %w", err)
	}
	var missing []string
	for _, name := range requiredCookies {
		if jar.m[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("cookies file is missing %s", strings.Join(missing, ", "))
	}
	return jar, nil
}

func (j *CookieJar) Get(name string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.m[name]
}

func (j *CookieJar) Header() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	names := make([]string, 0, len(j.m))
	for k := range j.m {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = k + "=" + j.m[k]
	}
	return strings.Join(parts, "; ")
}

// Update áp Set-Cookie của facebook.com rồi lưu lại file.
func (j *CookieJar) Update(h http.Header) {
	cookies := (&http.Response{Header: h}).Cookies()
	if len(cookies) == 0 {
		return
	}
	j.mu.Lock()
	changed := false
	for _, c := range cookies {
		if c.Domain != "" && !strings.HasSuffix(strings.TrimPrefix(c.Domain, "."), "facebook.com") {
			continue
		}
		expired := c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now())) || c.Value == "deleted"
		if expired {
			if _, ok := j.m[c.Name]; ok {
				delete(j.m, c.Name)
				changed = true
			}
		} else if j.m[c.Name] != c.Value {
			j.m[c.Name] = c.Value
			changed = true
		}
	}
	var data []byte
	if changed {
		data, _ = json.Marshal(j.m)
	}
	j.mu.Unlock()
	if data != nil {
		_ = writeFileAtomic(j.path, data)
	}
}

// State: cursor db 205 gần nhất và mốc cuối cùng còn theo kịp server (LiveUntilMs).
// Cursor không dùng lại được giữa các kết nối (thử thật 31/10/2026) nhưng vẫn thử
// trước; LiveUntilMs cho biết từ lúc nào phải lấy lại tin trong ảnh chụp khi nối lại.
type State struct {
	Cursor      string    `json:"cursor"`
	SavedAt     time.Time `json:"saved_at"`
	LiveUntilMs int64     `json:"live_until_ms,omitempty"`
}

func loadState(path string) State {
	var s State
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "read state:", err)
		}
		return s
	}
	_ = json.Unmarshal(data, &s)
	return s
}

func saveState(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}
