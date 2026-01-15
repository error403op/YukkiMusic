package platforms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

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
	Entries     []ytdlpInfo `json:"entries"`
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

/*
Cache key fix:
Audio and Video MUST be separate, otherwise audio cache is reused for video
which causes: "no valid video dimensions found"
*/
func cacheKey(track *state.Track) string {
	if track.Video {
		return track.ID + "_video"
	}
	return track.ID + "_audio"
}

func ensureDownloadsDir() error {
	return os.MkdirAll("downloads", 0755)
}

func checkDownloadedFile(id string) (string, error) {
	pattern := filepath.Join("downloads", id+".*")
	files, err := filepath.Glob(pattern)
	if err != nil || len(files) == 0 {
		return "", errors.New("no cache")
	}
	return files[0], nil
}

/*
Direct streaming via yt-dlp -g
Works for:
YouTube, Twitter/X, Instagram, Reddit, TikTok, many CDNs.
If it fails → fallback to real download.
*/
func (y *YtDlpDownloader) getDirectStreamURL(track *state.Track) (string, error) {
	args := []string{
		"-g",
		"--no-playlist",
		"--geo-bypass",
		"--no-check-certificate",
		"--prefer-free-formats",
		"--hls-prefer-ffmpeg",
	}

	if track.Video {
		args = append(args, "-f", "bestvideo*[height<=720]/best")
	} else {
		args = append(args, "-f", "bestaudio/best")
	}

	if y.isYouTubeURL(track.URL) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, track.URL)

	cmd := exec.Command("yt-dlp", args...)

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", err
	}

	streamURL := strings.TrimSpace(out.String())
	if streamURL == "" || !strings.HasPrefix(streamURL, "http") {
		return "", errors.New("invalid stream url")
	}

	return streamURL, nil
}

func (y *YtDlpDownloader) GetTracks(query string, video bool) ([]*state.Track, error) {
	info, err := y.extractMetadata(query)
	if err != nil {
		return nil, err
	}

	if info.IsLive {
		return nil, errors.New("live streams are not supported")
	}

	var tracks []*state.Track
	if len(info.Entries) > 0 {
		for _, entry := range info.Entries {
			if entry.IsLive {
				continue
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

/*
Download():
1. Try DIRECT STREAM first (fastest, no disk, no CPU)
2. If failed → fallback to real download
3. Cache is separated for audio/video
4. Universal format support
5. Telegram compatible containers
*/
func (y *YtDlpDownloader) Download(
	ctx context.Context,
	track *state.Track,
	_ *telegram.NewMessage,
) (string, error) {

	// 1) Try direct stream
	if streamURL, err := y.getDirectStreamURL(track); err == nil {
		gologging.InfoF("YtDlp: Using direct stream for %s", track.ID)
		return streamURL, nil
	}

	// 2) Fallback to cached download
	key := cacheKey(track)
	if path, err := checkDownloadedFile(key); err == nil {
		gologging.InfoF("YtDlp: Using cached file for %s", key)
		return path, nil
	}

	if err := ensureDownloadsDir(); err != nil {
		return "", err
	}

	outTpl := filepath.Join("downloads", key+".%(ext)s")

	// Universal stable arguments
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
		// Telegram safe: MP4, <=720p, no broken codecs
		args = append(args,
			"-f", "bestvideo*[height<=720][vcodec!=vp9]/best[height<=720]/best",
			"--merge-output-format", "mp4",
			"--remux-video", "mp4",
		)
	} else {
		// Best quality audio for Telegram voice chats
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

	if err := cmd.Run(); err != nil {
		gologging.ErrorF(
			"YtDlp error: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			err,
			stdout.String(),
			stderr.String(),
		)
		return "", fmt.Errorf("yt-dlp error: %w", err)
	}

	finalPath := strings.TrimSpace(stdout.String())
	if finalPath == "" {
		return "", errors.New("yt-dlp returned empty path")
	}

	if _, err := os.Stat(finalPath); err != nil {
		return "", err
	}

	gologging.InfoF("YtDlp: Downloaded %s", finalPath)
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

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("metadata extraction failed: %w\n%s", err, stderr.String())
	}

	var info ytdlpInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return nil, err
	}

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
