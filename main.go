package main

import (
	"io"
	"log"
	"net/http"
	"os"

	"api.groovio/downloader"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

func main() {
	app := pocketbase.New()

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {


		// POST /api/download endpoint
		se.Router.POST("/api/track-download", func(e *core.RequestEvent) error {
			var payload downloader.DownloadRequest
			if err := e.BindBody(&payload); err != nil {
				return e.JSON(http.StatusBadRequest, map[string]string{
					"error": "Invalid request body: " + err.Error(),
				})
			}

			// --- ASYNCHRONOUS HANDLING ---
			// Since downloading and tagging is a long-running process,
			// it's best to run it in a goroutine and return immediately.
			
			// Start the job in a background goroutine
			go func() {
				// The PocketBase app pointer is safe to use in a goroutine
				record, err := downloader.HandleDownload(app, payload)
				if err != nil {
					// Log the error for internal tracking
					log.Printf("Async download failed for track ID %s: %v", payload.SpotifyTrackID, err)
					return // Job failed, nothing more to do for this background task.
				}
				
				// Optional: You could implement a webhook or a live-update mechanism (like websockets)
				// to notify the client when the record (and file) is ready.
				log.Printf("Async download and upload complete for record ID: %s", record.Id)
			}()

			// Return an immediate success response indicating the job started.
			return e.JSON(http.StatusOK, map[string]string{
				"status": "Download job started successfully. Check 'tracks' collection for completion.",
				"track_id": payload.SpotifyTrackID,
			})
		})

		se.Router.GET("/api/play-track/{trackId}", func(e *core.RequestEvent) error {
			// 1. Get the Spotify ID from the URL path
			trackId := e.Request.PathValue("trackId")
			if trackId == "" {
					return e.JSON(http.StatusBadRequest, map[string]string{
						"error": "Missing trackId parameter",
					})
			}
		
			record, err := app.FindFirstRecordByData("tracks", "spotify_track_id", trackId)
			if err != nil {
				return e.JSON(http.StatusBadRequest, map[string]string{
					"error": "Track doesn't exist",
				})
			}

			fileName := record.GetString("file")
			if fileName == "" {
					return e.JSON(http.StatusNotFound, map[string]string{
							"error": "Track file not available",
					})
			}

			// pocketBaseURL := app.Settings().Meta.AppURL
			// fileKey := record.Collection().Id + "/" + record.Id + "/" + fileName
			fileURL := record.BaseFilesPath() + "/" + fileName + "?download=1"

			 // 3. Open the filesystem
			fsys, err := app.NewFilesystem()
			if err != nil {
					return e.JSON(http.StatusInternalServerError, map[string]string{
							"error": "Failed to open filesystem: " + err.Error(),
					})
			}
			defer fsys.Close()

			// 4. Get a reader for the file
			reader, err := fsys.GetReader(fileURL)
			if err != nil {
					return e.JSON(http.StatusNotFound, map[string]string{
							"error": "File not found",
					})
			}
			defer reader.Close()

			// 5. Set the correct content type (for mp3 audio)
			e.Response.Header().Set("Content-Type", "audio/mpeg")

			// 6. Check for download query
			download := e.Request.URL.Query().Get("download")
			if download == "1" {
					e.Response.Header().Set("Content-Disposition", "attachment; filename=\""+fileName+"\"")
			}

			// 7. Stream the file directly to the response
			_, err = io.Copy(e.Response, reader)
			if err != nil {
					return e.JSON(http.StatusInternalServerError, map[string]string{
							"error": "Failed to stream file: " + err.Error(),
					})
			}
			
			return nil
		})

		// Serve static files from pb_public
		se.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), false))


		// GET /downloads/{file} to serve audio files
		/*
		se.Router.GET("/downloads/{file}", func(e *core.RequestEvent) error {
			file := e.Request.PathValue("file")
			fullPath := filepath.Join("./downloads", file)
			filename := file

			f, err := os.Open(fullPath)
			if err != nil {
				return e.JSON(http.StatusNotFound, map[string]string{
					"error": "file not found",
				})
			}
			defer f.Close()

			// Get file info for size
			// stat, err := f.Stat()
			// if err != nil {
			// 		return e.JSON(http.StatusInternalServerError, map[string]string{
			// 				"error": "cannot stat file",
			// 		})
			// }

			// Serve the file
			e.Response.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
			e.Response.Header().Set("Content-Type", "audio/mpeg")
			// e.Response.Header().Set("Content-Length", fmt.Sprintf("%d", stat.Size()))

			http.ServeContent(e.Response, e.Request, file, time.Now(), f)
			return nil
		})
		*/

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
