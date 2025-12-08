package downloader

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bogem/id3v2"
	"github.com/google/uuid"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

type DownloadRequest struct {
	SpotifyTrackID string `json:"spotify_track_id"`
}

// ---- SPOTIFY API MODELS ----
type SpotifyTrack struct {
	Name        string `json:"name"`
	DurationMs  int    `json:"duration_ms"`
	TrackNumber int    `json:"track_number"`
	DiscNumber  int    `json:"disc_number"`
	ID          string `json:"id"`

	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`

	Album struct {
		Name        string `json:"name"`
		ReleaseDate string `json:"release_date"`
		Images      []struct {
			URL string `json:"url"`
		} `json:"images"`
	} `json:"album"`
}

func QueueTrack(app core.App, payload DownloadRequest) (*core.Record, error) {
	if payload.SpotifyTrackID == "" {
		return nil, errors.New("spotify_track_id is required")
	}

	// Check if track already exists - so we dont create duplicate requests
	_, err := app.FindFirstRecordByData("tracks", "spotify_track_id", payload.SpotifyTrackID)
	if err == nil {
		return nil, errors.New("track with provided spotify_track_id is already downloaded and ready to play")
	}

	// Get track metadata from Spotify
	spotifyTrack, err := fetchSpotifyMetadata(payload.SpotifyTrackID)
	if err != nil {
		return nil, fmt.Errorf("spotify metadata error: %w", err)
	}

	fmt.Printf("Fetched from Spotify: %s - %s\n", spotifyTrack.Name, spotifyTrack.Album.Name)

	// Create track record
	track, err := saveTrackRecord(app, spotifyTrack)
	if err != nil {
		return nil, fmt.Errorf("track save error: %w", err)
	}

	return track, nil
}

// ======================================================================
// MAIN HANDLER ENTRY
// ======================================================================
func DownloadTrack(app core.App, track *core.Record) (*core.Record, error) {
	// Download from youtube
	downloadDir := "./downloads"
	os.MkdirAll(downloadDir, os.ModePerm)

	fileID := uuid.New().String()
	tmpFile := filepath.Join(downloadDir, fmt.Sprintf("%s.mp3", fileID))

	cmd := createYTDLPCommand(track, tmpFile)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("yt-dlp failed: %w", err)
	}

	// Apply ID3 tags
	if err := writeID3Tags(track, tmpFile, fileID, downloadDir); err != nil {
		return nil, err
	}

	// Update record and save file to R2
	record, err := updateTrackRecord(app, track, tmpFile)
	if err != nil {
		return nil, fmt.Errorf("record save error: %w", err)
	}

	// delete local temp file
	os.Remove(tmpFile)

	return record, nil
}

// ======================================================================
//  SPOTIFY METADATA FETCH
// ======================================================================

func fetchSpotifyMetadata(trackID string) (*SpotifyTrack, error) {
	token, err := getSpotifyToken()
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequest("GET", "https://api.spotify.com/v1/tracks/"+trackID, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("spotify error %d: %s", resp.StatusCode, body)
	}

	var track SpotifyTrack
	json.NewDecoder(resp.Body).Decode(&track)

	return &track, nil
}

func getSpotifyToken() (string, error) {
	clientID := os.Getenv("SPOTIFY_CLIENT_ID")
	clientSecret := os.Getenv("SPOTIFY_CLIENT_SECRET")

	if clientID == "" || clientSecret == "" {
		return "", errors.New("missing spotify client id/secret env vars")
	}

	auth := base64.StdEncoding.EncodeToString([]byte(clientID + ":" + clientSecret))

	reqData := url.Values{}
	reqData.Set("grant_type", "client_credentials")

	req, _ := http.NewRequest("POST",
			"https://accounts.spotify.com/api/token",
			strings.NewReader(reqData.Encode()),
	)
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var data struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&data)

	return data.AccessToken, nil
}

// ======================================================================
//  YT-DLP COMMAND
// ======================================================================

func createYTDLPCommand(track *core.Record, tmpFile string) *exec.Cmd {
	search := fmt.Sprintf("%s %s", cleanTrackName(track.GetString("name")), track.GetString("artist"))

	desired := track.GetInt("duration") / 1000
	min := desired - 5
	max := desired + 5

	return exec.Command("yt-dlp",
		"--extract-audio",
		"--audio-format", "mp3",
		"--output", tmpFile,
		"--format", "bestaudio/best",
		"--no-playlist",
		"--match-filter", fmt.Sprintf("duration>%d & duration<%d", min, max),
		fmt.Sprintf("ytsearch10:%s", search),
	)
}

func cleanTrackName(n string) string {
	if i := strings.Index(n, "("); i != -1 {
		n = n[:i]
	}
	if i := strings.Index(n, "["); i != -1 {
		n = n[:i]
	}
	return strings.TrimSpace(n)
}

// ======================================================================
//  ID3 TAG WRITING
// ======================================================================

func writeID3Tags(track *core.Record, tmpFile, fileID, dir string) error {
	tag, err := id3v2.Open(tmpFile, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("id3 open error: %w", err)
	}
	defer tag.Close()

	tag.SetVersion(3)
	tag.SetTitle(track.GetString("name"))

	tag.SetArtist(track.GetString("artist"))
	tag.SetAlbum(track.GetString("album"))

	fullDate := track.GetString("release_date") // "2021-08-23"
	year := ""
	if len(fullDate) >= 4 {
			year = fullDate[:4] // "2021"
	}
	tag.SetYear(year)

	// Album art
	if len(track.GetString("cover_url")) > 0 {
		coverURL := track.GetString("cover_url")
		coverPath := filepath.Join(dir, fmt.Sprintf("%s_cover.jpg", fileID))
		if err := downloadFile(coverURL, coverPath); err == nil {
			imgBytes, _ := os.ReadFile(coverPath)
			tag.AddAttachedPicture(id3v2.PictureFrame{
				Encoding:    id3v2.EncodingUTF8,
				MimeType:    "image/jpeg",
				PictureType: id3v2.PTFrontCover,
				Picture:     imgBytes,
			})
			os.Remove(coverPath)
		}
	}

	return tag.Save()
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// ======================================================================
//  SAVE RECORD TO POCKETBASE
// ======================================================================

func saveTrackRecord(app core.App, t *SpotifyTrack) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId("tracks")
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(col)
	record.Set("download_status", "queued");

	// Spotify data
	record.Set("spotify_track_id", t.ID)
	record.Set("name", t.Name)
	record.Set("album", t.Album.Name)

	artistNames := []string{}
	for _, a := range t.Artists {
		artistNames = append(artistNames, a.Name)
	}
	record.Set("artist", strings.Join(artistNames, ", "))
	record.Set("duration", t.DurationMs)

	record.Set("release_date", t.Album.ReleaseDate)
	record.Set("track_number", t.TrackNumber)

	if len(t.Album.Images) > 0 {
		record.Set("cover_url", t.Album.Images[0].URL)
	}

	if err := app.Save(record); err != nil {
		return nil, err
	}

	return record, nil
}

func updateTrackRecord(app core.App, track *core.Record, localPath string) (*core.Record, error) {
	file, err := filesystem.NewFileFromPath(localPath)
	if err != nil {
		return nil, err
	}

	track.Set("file", file) // field name must match your schema
	if err := app.Save(track); err != nil {
		return nil, err
	}

	return track, nil
}
