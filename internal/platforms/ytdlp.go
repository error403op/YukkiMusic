package platforms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Laky-64/gologging"
	"github.com/amarnathcjd/gogram/telegram"

	"main/internal/cookies"
	state "main/internal/core/models"
)

const PlatformYtDlp state.PlatformName = "YtDlp"

type YtDlpDownloader struct {
	name state.PlatformName
}

type ytdlpInfo struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Duration    float64     `json:"duration"`
	Thumbnail   string      `json:"thumbnail"`
	URL         string      `json:"webpage_url"`
	OriginalURL string      `json:"original_url"`
	Uploader    string      `json:"uploader"`
	Description string      `json:"description"`
	IsLive      bool        `json:"is_live"`
	WasLive     bool        `json:"was_live"`
	Entries     []ytdlpInfo `json:"entries"`
}

var youtubePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(youtube\.com|youtu\.be|music\.youtube\.com)`),
}

func init() {
	Register(60, &YtDlpDownloader{name: PlatformYtDlp})
}

func (y *YtDlpDownloader) Name() state.PlatformName {
	return y.name
}

func (y *YtDlpDownloader) IsValid(query string) bool {
	query = strings.TrimSpace(query)
	u, err := url.Parse(query)
	return err == nil && u.Scheme != "" && u.Host != ""
}

/* ---------- CACHE KEY FIX ---------- */

func cacheKey(track *state.Track) string {
	if track.Video {
		return track.ID + "_video"
	}
	return track.ID + "_audio"
}

/* ---------- STREAM VALIDATION ---------- */

func validateStreamURL(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, "HEAD", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

/* ---------- DIRECT STREAM ---------- */

func (y *YtDlpDownloader) getDirectStreamURL(ctx context.Context, track *state.Track) (string, error) {
	args := []string{
		"-g",
		"--no-playlist",
		"--geo-bypass",
		"--no-check-certificate",
		"--no-warnings",
	}

	if track.Video {
		args = append(args, "-f",
			"bestvideo*[height<=720][protocol!=m3u8]/best[height<=720][protocol!=m3u8]/best")
	} else {
		args = append(args, "-f",
			"bestaudio[protocol!=m3u8]/bestaudio")
	}

	if y.isYouTubeURL(track.URL) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, track.URL)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp -g failed: %s", errBuf.String())
	}

	lines := strings.Split(out.String(), "\n")
	for _, u := range lines {
		u = strings.TrimSpace(u)
		if strings.HasPrefix(u, "http") {
			if err := validateStreamURL(ctx, u); err == nil {
				return u, nil
			}
		}
	}
	return "", errors.New("no valid direct stream url")
}

/* ---------- MAIN DOWNLOAD ---------- */

func (y *YtDlpDownloader) Download(ctx context.Context, track *state.Track, msg *telegram.NewMessage) (string, error) {

	// 1. Direct streaming always first
	if url, err := y.getDirectStreamURL(ctx, track); err == nil {
		return url, nil
	}

	if track.IsLive {
		return "", errors.New("live streams cannot be downloaded as file")
	}

	key := cacheKey(track)
	if path, err := checkDownloadedFile(key); err == nil {
		// Validate cache
		if track.Video {
			if isValidVideoFile(path) {
				return path, nil
			}
		} else {
			return path, nil
		}
		os.Remove(path)
	}

	if err := ensureDownloadsDir(); err != nil {
		return "", err
	}

	outTpl := filepath.Join("downloads", key+".%(ext)s")

	args := []string{
		"--no-playlist",
		"--no-part",
		"--force-overwrites",
		"--no-warnings",
		"--geo-bypass",
		"--concurrent-fragments", "4",
		"--fragment-retries", "10",
		"--retries", "5",
		"--print", "after_move:filepath",
		"-o", outTpl,
	}

	if track.Video {
		args = append(args,
			"-f", "bestvideo*[height<=720][vcodec!=vp9]/best[height<=720]/best",
			"--merge-output-format", "mp4",
		)
	} else {
		args = append(args,
			"-f", "bestaudio",
			"--extract-audio",
			"--audio-format", "opus",
		)
	}

	if y.isYouTubeURL(track.URL) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, track.URL)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("yt-dlp failed: %s", stderr.String())
	}

	path := strings.TrimSpace(stdout.String())
	if path == "" {
		return "", errors.New("yt-dlp produced empty output path")
	}

	if _, err := os.Stat(path); err != nil {
		return "", err
	}

	return path, nil
}

/* ---------- VIDEO VALIDATION ---------- */

func isValidVideoFile(path string) bool {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

/* ---------- METADATA ---------- */

func (y *YtDlpDownloader) extractMetadata(urlStr string) (*ytdlpInfo, error) {
	args := []string{"-j", "--no-warnings"}

	if y.isYouTubeURL(urlStr) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, urlStr)

	cmd := exec.Command("yt-dlp", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var info ytdlpInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return nil, err
	}
	return &info, nil
}

/* ---------- UTILS ---------- */

func (y *YtDlpDownloader) isYouTubeURL(u string) bool {
	for _, p := range youtubePatterns {
		if p.MatchString(u) {
			return true
		}
	}
	return false
}
