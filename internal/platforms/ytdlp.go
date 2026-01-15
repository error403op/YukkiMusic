package platforms

import (
	"context"
	"encoding/json"
	"errors"
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
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		return false
	}
	return true
}

func cacheKey(track *state.Track) string {
	if track.Video {
		return track.ID + "_video"
	}
	return track.ID + "_audio"
}

func isValidVideoFile(path string) bool {
	cmd := exec.Command(
		"ffprobe",
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

func (y *YtDlpDownloader) GetTracks(query string, video bool) ([]*state.Track, error) {
	query = strings.TrimSpace(query)
	gologging.InfoF("YtDlp: Extracting metadata for %s", query)

	info, err := y.extractMetadata(query)
	if err != nil {
		return nil, err
	}

	if info.IsLive {
		return nil, errors.New("live streams are not supported by yt-dlp downloader")
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

func (y *YtDlpDownloader) Download(
	ctx context.Context,
	track *state.Track,
	_ *telegram.NewMessage,
) (string, error) {

	key := cacheKey(track)

	// Cache check
	if path, err := checkDownloadedFile(key); err == nil {
		if track.Video {
			if isValidVideoFile(path) {
				gologging.InfoF("YtDlp: Using cached video file %s", path)
				return path, nil
			}
			gologging.WarnF("YtDlp: Cached video invalid, deleting: %s", path)
			_ = os.Remove(path)
		} else {
			gologging.InfoF("YtDlp: Using cached audio file %s", path)
			return path, nil
		}
	}

	if err := ensureDownloadsDir(); err != nil {
		return "", err
	}

	outTpl := filepath.Join("downloads", key+".%(ext)s")

	args := []string{
		"--no-playlist",
		"--geo-bypass",
		"--no-part",
		"--force-overwrites",
		"--no-check-certificate",

		"--retries", "5",
		"--fragment-retries", "5",
		"--file-access-retries", "5",
		"--extractor-retries", "3",

		"--hls-prefer-ffmpeg",
		"--downloader", "ffmpeg",

		"--print", "after_move:filepath",
		"-o", outTpl,

		"--verbose",
	}

	if y.isYouTubeURL(track.URL) {
		if track.Video {
			args = append(args,
				"-f", "(bv*[height<=720]/bv*)+(ba/best)",
				"--merge-output-format", "mp4",
			)
		} else {
			args = append(args,
				"-f", "bestaudio",
				"--extract-audio",
				"--audio-format", "opus",
			)
		}
	} else {
		if track.Video {
			args = append(args, "-f", "bestvideo+bestaudio/best")
		} else {
			args = append(args, "-f", "bestaudio/best")
		}
	}

	if y.isYouTubeURL(track.URL) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, track.URL)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)

	// Strongest debug capture
	out, err := cmd.CombinedOutput()
	if err != nil {
		exitCode := -1
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}

		gologging.ErrorF(
			"YtDlp: Download failed\n"+
				"Exit code: %d\n"+
				"Command: yt-dlp %v\n"+
				"Output:\n%s",
			exitCode,
			args,
			string(out),
		)

		return "", err
	}

	finalPath := strings.TrimSpace(string(out))
	if finalPath == "" {
		return "", errors.New("yt-dlp returned empty file path")
	}

	if _, err := os.Stat(finalPath); err != nil {
		return "", err
	}

	gologging.InfoF("YtDlp: Download complete: %s", finalPath)
	return finalPath, nil
}

func (y *YtDlpDownloader) extractMetadata(urlStr string) (*ytdlpInfo, error) {
	args := []string{
		"-j",
		"--no-check-certificate",
		"--verbose",
	}

	if y.isYouTubeURL(urlStr) {
		if cookie, err := cookies.GetRandomCookieFile(); err == nil && cookie != "" {
			args = append(args, "--cookies", cookie)
		}
	}

	args = append(args, urlStr)

	cmd := exec.Command("yt-dlp", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		gologging.ErrorF(
			"YtDlp: Metadata extraction failed\nCommand: yt-dlp %v\nOutput:\n%s",
			args,
			string(out),
		)
		return nil, err
	}

	var info ytdlpInfo
	if err := json.Unmarshal(out, &info); err != nil {
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
