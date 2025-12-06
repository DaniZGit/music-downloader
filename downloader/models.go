// // downloader/models.go (Create this file or add these to handler.go if simple)

package downloader

// // --- Request Payload ---

// type DownloadRequest struct {
// 	SpotifyTrackID string `json:"spotify_track_id"`
// }

// // --- Spotify API Response Structs (Simplified) ---
// // These are simplified to only include fields relevant to your process.

// type SpotifyTrack struct {
// 	ID           string           `json:"id"`
// 	Name         string           `json:"name"`
// 	DurationMs   int              `json:"duration_ms"`
// 	Explicit     bool             `json:"explicit"`
// 	Popularity   int              `json:"popularity"`
// 	TrackNumber  int              `json:"track_number"`
// 	DiscNumber   int              `json:"disc_number"`
// 	Artists      []SpotifyArtist  `json:"artists"`
// 	Album        *SpotifyAlbum    `json:"album"`
// 	ExternalIDs  map[string]string `json:"external_ids"` // For ISRC
// }

// type SpotifyArtist struct {
// 	Name string `json:"name"`
// }

// type SpotifyAlbum struct {
// 	Name         string         `json:"name"`
// 	AlbumType    string         `json:"album_type"`
// 	ReleaseDate  string         `json:"release_date"` // YYYY-MM-DD format
// 	Label        string         `json:"label"`
// 	Images       []SpotifyImage `json:"images"`
// 	// Genres are usually on the Album endpoint, not the Track endpoint.
// 	// You might need an extra call to get them, but let's assume they might be missing or on the track if you get a full track object.
// 	// We'll leave it out for simplicity, but note that Spotify's 'simplified' track object in search doesn't have genres.
// 	Genres []string `json:"genres"`
// }

// type SpotifyImage struct {
// 	URL    string `json:"url"`
// 	Height int    `json:"height"`
// 	Width  int    `json:"width"`
// }

// // --- PocketBase Record Struct ---
// // Define a struct matching your PocketBase 'tracks' collection fields.
// // This is what you'll insert into the database.
// type TrackRecord struct {
// 	SpotifyTrackID string `json:"spotify_track_id"`
// 	Title          string `json:"title"`
// 	Album          string `json:"album"`
// 	Artist         string `json:"artist"` // Comma-separated artists
// 	Duration       int    `json:"duration"` // Seconds
// 	File           string `json:"file"` // The S3 file path/name
// }