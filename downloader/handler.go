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
	"strconv"
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

// ======================================================================
// MAIN HANDLER ENTRY
// ======================================================================

func HandleDownload(app core.App, payload DownloadRequest) (*core.Record, error) {
	if payload.SpotifyTrackID == "" {
		return nil, errors.New("spotify_track_id is required")
	}

	// -----------------------------------------------------
	// 0) Check if track has already been downloaded
	// -----------------------------------------------------
	_, err := app.FindFirstRecordByData("tracks", "spotify_track_id", payload.SpotifyTrackID)
	if err == nil {
		return nil, errors.New("track with provided spotify_track_id is already downloaded and ready to play")
	}

	// -----------------------------------------------------
	// 1) GET TRACK METADATA FROM SPOTIFY
	// -----------------------------------------------------

	track, err := fetchSpotifyMetadata(payload.SpotifyTrackID)
	if err != nil {
		return nil, fmt.Errorf("spotify metadata error: %w", err)
	}

	fmt.Printf("Fetched from Spotify: %s - %s\n", track.Name, track.Album.Name)

	// -----------------------------------------------------
	// 2) DOWNLOAD FROM YOUTUBE
	// -----------------------------------------------------

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

	// -----------------------------------------------------
	// 3) APPLY ID3 TAGS
	// -----------------------------------------------------

	if err := writeID3Tags(track, tmpFile, fileID, downloadDir); err != nil {
		return nil, err
	}

	// -----------------------------------------------------
	// 4) UPLOAD TO POCKETBASE (CLOUDFLARE R2)
	// -----------------------------------------------------

	record, err := saveTrackRecord(app, track, tmpFile)
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

	fmt.Println("POST body:", reqData.Encode())
	fmt.Println("Headers:", req.Header)

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

func createYTDLPCommand(track *SpotifyTrack, tmpFile string) *exec.Cmd {
	search := fmt.Sprintf("%s %s", cleanTrackName(track.Name), track.Artists[0].Name)

	desired := track.DurationMs / 1000
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

func writeID3Tags(track *SpotifyTrack, tmpFile, fileID, dir string) error {
	tag, err := id3v2.Open(tmpFile, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("id3 open error: %w", err)
	}
	defer tag.Close()

	tag.SetVersion(3)
	tag.SetTitle(track.Name)

	artists := []string{}
	for _, a := range track.Artists {
		artists = append(artists, a.Name)
	}
	tag.SetArtist(strings.Join(artists, ", "))
	tag.SetAlbum(track.Album.Name)

	tag.AddTextFrame(tag.CommonID("TRCK"), tag.DefaultEncoding(), strconv.Itoa(track.TrackNumber))
	tag.AddTextFrame(tag.CommonID("TPOS"), tag.DefaultEncoding(), strconv.Itoa(track.DiscNumber))
	tag.AddTextFrame(tag.CommonID("TDRC"), tag.DefaultEncoding(), track.Album.ReleaseDate)

	// Album art
	if len(track.Album.Images) > 0 {
		coverURL := track.Album.Images[0].URL
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

func saveTrackRecord(app core.App, t *SpotifyTrack, localPath string) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId("tracks")
	if err != nil {
		return nil, err
	}

	record := core.NewRecord(col)

	record.Set("spotify_track_id", t.ID)
	record.Set("title", t.Name)
	record.Set("album", t.Album.Name)

	artistNames := []string{}
	for _, a := range t.Artists {
		artistNames = append(artistNames, a.Name)
	}
	record.Set("artist", strings.Join(artistNames, ", "))

	record.Set("duration", t.DurationMs)

	file, err := filesystem.NewFileFromPath(localPath)
	if err != nil {
		return nil, err
	}

	record.Set("file", file) // field name must match your schema

	if err := app.Save(record); err != nil {
		return nil, err
	}

	return record, nil
}
