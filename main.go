package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"api.groovio/downloader"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

var downloadSemaphore = make(chan struct{}, 2)

func main() {
	app := pocketbase.New()

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
				log.Printf("Track was succesfully added to the queue. Queue record ID: %s", record.Id)
			}()

			// Return an immediate success response indicating the job started.
			return e.JSON(http.StatusOK, map[string]string{
				"status": "Track was added to the queue",
			})
		})

		// 2. Register cron job for downloading queued tracks
		app.Cron().MustAdd("queue_worker", "*/1 * * * *", func() {
			// Find queued jobs
			jobs, err := app.FindRecordsByFilter(
				"queued_tracks",
				"status='queued' && track_id!=null",
				"-created",
				cap(downloadSemaphore),
				0,
			)
			if err != nil {
				log.Println("Could not fetch queue:", err)
				return
			}

			for _, job := range jobs {
				// mark job as downloading
				job.Set("status", "downloading")
				app.Save(job)

				// fire worker
				go func(job *core.Record) {
					downloadSemaphore <- struct{}{} // acquire slot
					defer func() { <-downloadSemaphore }() // release slot

					errs := app.ExpandRecord(job, []string{"track_id"}, nil)
					if len(errs) > 0 {
						fmt.Printf("failed to expand queue job: %v", errs)
						return;
					}

					_, err = downloader.DownloadTrack(app, job.ExpandedOne("track_id"))
					if err != nil {
						job.Set("status", "failed")
						fmt.Printf("Failed to download track %s", err.Error())
					} else {
						job.Set("status", "completed")
					}

					if err := app.Save(job); err != nil {
						log.Println("Failed to update queue item:", err)
					}

				}(job)
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

		// Serve static files from pb_public
		se.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), false))

		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
