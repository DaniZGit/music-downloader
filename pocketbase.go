package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"api.groovio/downloader"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"

	// For migrations to be auto run
	_ "api.groovio/migrations"
)

var downloadSemaphore = make(chan struct{}, 2)

func main() {
	app := pocketbase.New()

	// Detect if invoked via "go run .", so automigrate only runs in dev.
	isGoRun := strings.HasPrefix(os.Args[0], os.TempDir())

	// Register the migrate plugin. This adds the "migrate" subcommands to the CLI.
	// Automigrate: when true, changes from Dashboard will auto-generate migration files.
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: isGoRun,
	})

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {

		// 1. Queue the track for download
		se.Router.POST("/api/queue-track", func(e *core.RequestEvent) error {
			var payload downloader.DownloadRequest
			if err := e.BindBody(&payload); err != nil {
				return e.JSON(http.StatusBadRequest, map[string]string{
					"error": "Invalid request body: " + err.Error(),
				})
			}

			// Start the job in a background goroutine
			go func() {
				// The PocketBase app pointer is safe to use in a goroutine
				record, err := downloader.QueueTrack(app, payload)
				if err != nil {
					// Log the error for internal tracking
					log.Printf("Adding track to the queue FAILED for track ID %s: %v", payload.SpotifyTrackID, err)
					return // Job failed, nothing more to do for this background task.
				}
				
				// Optional: You could implement a webhook or a live-update mechanism (like websockets)
				// to notify the client when the record (and file) is ready.
				log.Printf("Track was succesfully added to the queue. Track ID: %s", record.Id)
			}()

			// Return an immediate success response indicating the job started.
			return e.JSON(http.StatusOK, map[string]string{
				"status": "Track was added to the queue",
			})
		})

		// 2. Register cron job for downloading queued tracks
		app.Cron().MustAdd("queue_worker", "*/1 * * * *", func() {
			// Find queued tracks
			tracks, err := app.FindRecordsByFilter(
				"tracks",
				"download_status='queued'",
				"-created",
				cap(downloadSemaphore),
				0,
			)
			if err != nil {
				// No rows returned
				return
			}

			for _, track := range tracks {
				// mark job as downloading
				track.Set("download_status", "downloading")
				app.Save(track)

				// fire worker
				go func(track *core.Record) {
					downloadSemaphore <- struct{}{} // acquire slot
					defer func() { <-downloadSemaphore }() // release slot

					_, err = downloader.DownloadTrack(app, track)
					if err != nil {
						track.Set("download_status", "failed")
						fmt.Printf("Failed to download track %s", err.Error())
					} else {
						track.Set("download_status", "completed")
					}

					if err := app.Save(track); err != nil {
						log.Println("Failed to update queued track status:", err)
					}

				}(track)
			}
		})

		// 3. Expose endpoint for playing/download tracks
		se.Router.GET("/api/play-track/{spotifyTrackId}", func(e *core.RequestEvent) error {
			// 1. Get the Spotify ID from the URL path
			spotifyTrackId := e.Request.PathValue("spotifyTrackId")
			if spotifyTrackId == "" {
					return e.JSON(http.StatusBadRequest, map[string]string{
						"error": "Missing spotifyTrackId parameter",
					})
			}
		
			record, err := app.FindFirstRecordByData("tracks", "spotify_track_id", spotifyTrackId)
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
					if !strings.Contains(err.Error(), "broken pipe") {
							return e.JSON(http.StatusInternalServerError, map[string]string{
									"error": "Failed to stream file: " + err.Error(),
							})
					}
					// else: client disconnected, ignore
			}
			
			return nil
		})

		// 4. Expose endpoint for checking if tracks audio exist
		se.Router.GET("/api/check-tracks", func(e *core.RequestEvent) error {
			trackIdsQuery := e.Request.URL.Query().Get("ids")
			trackIds := strings.Split(trackIdsQuery, ",")

			// Build filter
			filters := make([]string, len(trackIds))
			params := make(dbx.Params)
			for i, id := range trackIds {
					key := fmt.Sprintf("id%d", i)
					filters[i] = "spotify_track_id = {:" + key + "}"
					params[key] = strings.TrimSpace(id)
			}

			// Seperate filters with OR operator
			filter := strings.Join(filters, " || ")

			// Fetch tracks
			tracks, _ := app.FindRecordsByFilter(
					"tracks",    // collection
					filter,      // filter string
					"",          // sort
					0,           // limit 0 = no limit
					0,           // offset
					params,
			)

			// Map tracks to spotify_track_id <-> download_status
			mappedTracks := make(map[string]string)
			for _, id := range trackIds {
					mappedTracks[strings.TrimSpace(id)] = "failed" // default
			}

			for _, track := range tracks {
					status := track.GetString("download_status")
					if status != "" {
							spotifyTrackId := track.GetString("spotify_track_id")
							mappedTracks[spotifyTrackId] = status
					}
			}

			return e.JSON(http.StatusOK, map[string]any{
				"tracks": mappedTracks,
			})
		})

		// Serve static files from pb_public
		se.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), false))

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
