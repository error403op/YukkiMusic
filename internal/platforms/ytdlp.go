package platforms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	WasLive     bool        `json:"was_live"` // past live streams
	Entries     []ytdlpInfo `json:"entries"`
	Formats     []struct {
		URL    string `json:"url"`
		Format string `json:"format_note"`
		Ext    string `json:"ext"`
	} `json:"formats"`
}

var youtubePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(youtube\.com|youtu\.be|music\.youtube\.com)`),
}

func init() {
	Register(60, &YtDlpDownloader{
		name: PlatformYtDlp,
	})
}

func (y *YtDlpDownloader) Name() state.PlatformName {
	return y.name
}

func (y *YtDlpDownloader) IsValid(query string) bool {
	query = strings.TrimSpace(query)
	parsedURL, err := url.Parse(query)
	return err == nil && parsedURL.Scheme != "" && parsedURL.Host != ""
}

func cacheKey(track *state.Track) string {
	if track.Video {
		return track.ID + "_video"
	}
	return track.ID + "_audio"
}

// validateStreamURL checks if the URL is reachable and returns content-type
func validateStreamURL(ctx context.Context, u string) error {
	req, err := http.NewRequestWithContext(ctx, "HEAD", u, nil)
	if err != nil {
		return fmt.Errorf("failed to create HEAD request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; yt-dlp/2023.07.06)")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("stream unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("stream returned HTTP %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	gologging.DebugF("Stream URL validated | Content-Type: %s", contentType)
	return nil
}

func (y *YtDlpDownloader) getDirectStreamURL(ctx context.Context, track *state.Track) (string, error) {
	args := []string{
		"-g",
		"--no-playlist",
		"--geo-bypass",
		"--no-check-certificate",
		"--prefer-free-formats",
		"--no-warnings",
	}

	if y.isYouTubeURL(track.URL) {
		if track.Video {
			args = append(args,
				"-f", "bestvideo*[protocol!=m3u8][height<=720]/best[protocol!=m3u8]",
			)
		} else {
			args = append(args,
				"-f", "bestaudio[protocol!=m3u8]/bestaudio",
			)
		}
	} else {
		if track.Video {
			args = append(args, "-f", "bestvideo*[height<=720]/best")
		} else {
			args = append(args, "-f", "bestaudio/best")
		}
	}

	if y.isYouTubeURL(track.URL) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
			gologging.DebugF("Using cookie file: %s", cookie)
		}
	}

	args = append(args, track.URL)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	gologging.DebugF("Executing yt-dlp for direct stream: %v", args)

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		gologging.ErrorF("yt-dlp -g failed after %v\nArgs: %v\nStderr:\n%s\nError: %v",
			duration, args, stderr.String(), err)
		return "", fmt.Errorf("yt-dlp stream extraction failed: %w", err)
	}

	streamURL := strings.TrimSpace(out.String())
	if streamURL == "" {
		gologging.WarnF("yt-dlp returned empty stream URL for %s", track.URL)
		return "", errors.New("empty stream URL from yt-dlp")
	}

	urls := strings.Split(streamURL, "\n")
	for i, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || !strings.HasPrefix(u, "http") {
			continue
		}
		gologging.InfoF("Validating candidate stream URL #%d: %s", i+1, u)
		if err := validateStreamURL(ctx, u); err == nil {
			gologging.InfoF("✅ Valid stream URL selected: %s", u)
			return u, nil
		}
		gologging.WarnF("Stream URL #%d invalid: %v", i+1, err)
	}

	return "", errors.New("no valid stream URLs returned by yt-dlp")
}

func (y *YtDlpDownloader) GetTracks(query string, video bool) ([]*state.Track, error) {
	info, err := y.extractMetadata(query)
	if err != nil {
		return nil, fmt.Errorf("failed to extract metadata: %w", err)
	}

	// Handle live or upcoming
	if info.IsLive {
		gologging.InfoF("Detected live stream: %s (ID: %s)", info.Title, info.ID)
		// We now SUPPORT live streams via direct URL!
		return []*state.Track{y.infoToTrack(info, video)}, nil
	}
	if info.WasLive {
		gologging.InfoF("Detected past live stream (VOD): %s", info.Title)
	}

	var tracks []*state.Track
	if len(info.Entries) > 0 {
		gologging.InfoF("Playlist detected with %d entries", len(info.Entries))
		for _, entry := range info.Entries {
			if entry.IsLive {
				gologging.InfoF("Including live entry in playlist: %s", entry.Title)
			}
			tracks = append(tracks, y.infoToTrack(&entry, video))
		}
	} else {
		tracks = append(tracks, y.infoToTrack(info, video))
	}

	return tracks, nil
}

func (y *YtDlpDownloader) IsDownloadSupported(source state.PlatformName) bool {
	return source == y.name || source == PlatformYouTube
}

func (y *YtDlpDownloader) Download(
	ctx context.Context,
	track *state.Track,
	msg *telegram.NewMessage,
) (string, error) {

	// Step 1: Try direct streaming (even for live!)
	gologging.InfoF("Attempting direct stream for track: %s (Video=%v, IsLive=%v)",
		track.ID, track.Video, track.IsLive)

	if streamURL, err := y.getDirectStreamURL(ctx, track); err == nil {
		gologging.InfoF("✅ Direct stream succeeded for %s → %s", track.ID, streamURL)
		return streamURL, nil
	} else {
		gologging.WarnF("Direct stream failed for %s: %v", track.ID, err)
	}

	// Step 2: If NOT live, try cached/download
	if track.IsLive {
		return "", errors.New("live stream cannot be downloaded as file; only direct streaming supported")
	}

	key := cacheKey(track)
	if path, err := checkDownloadedFile(key); err == nil {
		gologging.InfoF("Using cached file: %s", path)
		return path, nil
	}

	if err := ensureDownloadsDir(); err != nil {
		return "", fmt.Errorf("failed to create downloads dir: %w", err)
	}

	outTpl := filepath.Join("downloads", key+".%(ext)s")

	args := []string{
		"--no-playlist",
		"--no-part",
		"--no-overwrites",
		"--no-warnings",
		"--geo-bypass",
		"--ignore-errors",
		"--no-check-certificate",
		"--prefer-free-formats",
		"--force-overwrites",
		"--concurrent-fragments", "4",
		"--fragment-retries", "10",
		"--retries", "5",
		"--file-access-retries", "5",
		"--extractor-retries", "3",
		"--hls-prefer-ffmpeg",
		"--hls-use-mpegts",
		"--downloader", "ffmpeg",
		"--no-mtime",
		"--print", "after_move:filepath",
		"-o", outTpl,
	}

	if track.Video {
		args = append(args,
			"-f", "bestvideo*[height<=720][vcodec!=vp9]/best[height<=720]/best",
			"--merge-output-format", "mp4",
			"--remux-video", "mp4",
		)
	} else {
		args = append(args,
			"-f", "bestaudio[acodec=opus]/bestaudio/best",
			"--extract-audio",
			"--audio-format", "opus",
			"--audio-quality", "0",
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

	gologging.InfoF("Starting full download with args: %v", args)

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		stdoutStr := stdout.String()
		stderrStr := stderr.String()

		// Log everything
		gologging.ErrorF(`
❌ yt-dlp download FAILED after %v
Track ID: %s
URL: %s
Args: %v
STDOUT:
%s
STDERR:
%s
Final Error: %v`,
			duration, track.ID, track.URL, args, stdoutStr, stderrStr, err)

		// Check if context was cancelled
		if ctx.Err() == context.Canceled {
			return "", errors.New("download cancelled by user")
		}

		return "", fmt.Errorf("yt-dlp download failed: %w", err)
	}

	finalPath := strings.TrimSpace(stdout.String())
	if finalPath == "" {
		return "", errors.New("yt-dlp did not output a file path")
	}

	if _, err := os.Stat(finalPath); err != nil {
		return "", fmt.Errorf("downloaded file missing at %s: %w", finalPath, err)
	}

	fileInfo, _ := os.Stat(finalPath)
	gologging.InfoF("✅ Download complete: %s (%.2f MB) in %v",
		finalPath, float64(fileInfo.Size())/1024/1024, duration)

	return finalPath, nil
}

func (y *YtDlpDownloader) extractMetadata(urlStr string) (*ytdlpInfo, error) {
	args := []string{"-j", "--no-warnings"}

	if y.isYouTubeURL(urlStr) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, urlStr)

	cmd := exec.Command("yt-dlp", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	gologging.DebugF("Extracting metadata with: yt-dlp %v", args)

	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		gologging.ErrorF("Metadata extraction failed:\nURL: %s\nStderr:\n%s\nError: %v",
			urlStr, stderrStr, err)
		return nil, fmt.Errorf("metadata extraction failed: %w\nStderr:\n%s", err, stderrStr)
	}

	var info ytdlpInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		gologging.ErrorF("Failed to parse yt-dlp JSON:\n%s\nError: %v", stdout.String(), err)
		return nil, fmt.Errorf("invalid JSON from yt-dlp: %w", err)
	}

	gologging.DebugF("Metadata extracted: ID=%s, Title=%s, IsLive=%v, WasLive=%v",
		info.ID, info.Title, info.IsLive, info.WasLive)

	return &info, nil
}

func (y *YtDlpDownloader) infoToTrack(info *ytdlpInfo, video bool) *state.Track {
	url := info.URL
	if info.OriginalURL != "" {
		url = info.OriginalURL
	}

	return &state.Track{
		ID:       info.ID,
		Title:    info.Title,
		Duration: int(info.Duration),
		Artwork:  info.Thumbnail,
		URL:      url,
		Source:   PlatformYtDlp,
		Video:    video,
		IsLive:   info.IsLive, // ← Critical: expose live status!
	}
}

func (y *YtDlpDownloader) isYouTubeURL(urlStr string) bool {
	for _, p := range youtubePatterns {
		if p.MatchString(urlStr) {
			return true
		}
	}
	return false
}
