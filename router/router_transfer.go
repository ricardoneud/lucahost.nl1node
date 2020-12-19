package router

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"emperror.dev/errors"
	"encoding/hex"
	"fmt"
	"github.com/apex/log"
	"github.com/buger/jsonparser"
	"github.com/gin-gonic/gin"
	"github.com/juju/ratelimit"
	"github.com/mholt/archiver/v3"
	"github.com/pterodactyl/wings/api"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/installer"
	"github.com/pterodactyl/wings/router/tokens"
	"github.com/pterodactyl/wings/server"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func getServerArchive(c *gin.Context) {
	auth := strings.SplitN(c.GetHeader("Authorization"), " ", 2)

	if len(auth) != 2 || auth[0] != "Bearer" {
		c.Header("WWW-Authenticate", "Bearer")
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "The required authorization heads were not present in the request.",
		})
		return
	}

	token := tokens.TransferPayload{}
	if err := tokens.ParseToken([]byte(auth[1]), &token); err != nil {
		NewTrackedError(err).Abort(c)
		return
	}

	if token.Subject != c.Param("server") {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": "( .. •˘___˘• .. )",
		})
		return
	}

	s := GetServer(c.Param("server"))

	st, err := s.Archiver.Stat()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			NewServerError(err, s).SetMessage("failed to stat archive").Abort(c)
			return
		}

		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	checksum, err := s.Archiver.Checksum()
	if err != nil {
		NewServerError(err, s).SetMessage("failed to calculate checksum").Abort(c)
		return
	}

	file, err := os.Open(s.Archiver.Path())
	if err != nil {
		tserr := NewServerError(err, s)
		if !os.IsNotExist(err) {
			tserr.SetMessage("failed to open archive for reading")
		} else {
			tserr.SetMessage("failed to open archive")
		}

		tserr.Abort(c)
		return
	}
	defer file.Close()

	c.Header("X-Checksum", checksum)
	c.Header("X-Mime-Type", st.Mimetype)
	c.Header("Content-Length", strconv.Itoa(int(st.Info.Size())))
	c.Header("Content-Disposition", "attachment; filename="+s.Archiver.Name())
	c.Header("Content-Type", "application/octet-stream")

	bufio.NewReader(file).WriteTo(c.Writer)
}

func postServerArchive(c *gin.Context) {
	s := GetServer(c.Param("server"))

	go func(s *server.Server) {
		r := api.New()
		l := log.WithField("server", s.Id())

		// This function automatically adds the Source Node prefix and Timestamp to the log output before sending it
		// over the websocket.
		sendTransferLog := func(data string) {
			s.Events().Publish(server.TransferLogsEvent, "\x1b[0;90m"+time.Now().Format(time.RFC1123)+"\x1b[0m \x1b[1;33m[Source Node]:\x1b[0m "+data)
		}

		s.Events().Publish(server.TransferStatusEvent, "starting")
		sendTransferLog("Attempting to archive server..")

		hasError := true
		defer func() {
			if !hasError {
				return
			}

			s.Events().Publish(server.TransferStatusEvent, "failure")

			sendTransferLog("Attempting to notify panel of archive failure..")
			if err := r.SendArchiveStatus(s.Id(), false); err != nil {
				if !api.IsRequestError(err) {
					sendTransferLog("Failed to notify panel of archive failure: " + err.Error())
					l.WithField("error", err).Error("failed to notify panel of failed archive status")
					return
				}

				sendTransferLog("Panel returned an error while notifying it of a failed archive: " + err.Error())
				l.WithField("error", err.Error()).Error("panel returned an error when notifying it of a failed archive status")
				return
			}

			sendTransferLog("Successfully notified panel of failed archive status")
			l.Info("successfully notified panel of failed archive status")
		}()

		// Mark the server as transferring to prevent problems.
		s.SetTransferring(true)

		// Ensure the server is offline.
		if err := s.Environment.WaitForStop(60, false); err != nil {
			// Sometimes a "No such container" error gets through which means the server is already stopped.
			if !strings.Contains(err.Error(), "No such container") {
				sendTransferLog("Failed to stop server, aborting transfer..")
				l.WithField("error", err).Error("failed to stop server")
				return
			}
		}

		// Attempt to get an archive of the server.
		if err := s.Archiver.Archive(); err != nil {
			sendTransferLog("An error occurred while archiving the server: " + err.Error())
			l.WithField("error", err).Error("failed to get transfer archive for server")
			return
		}

		sendTransferLog("Successfully created archive, attempting to notify panel..")
		l.Info("successfully created server transfer archive, notifying panel..")

		if err := r.SendArchiveStatus(s.Id(), true); err != nil {
			if !api.IsRequestError(err) {
				sendTransferLog("Failed to notify panel of archive success: " + err.Error())
				l.WithField("error", err).Error("failed to notify panel of successful archive status")
				return
			}

			sendTransferLog("Panel returned an error while notifying it of a successful archive: " + err.Error())
			l.WithField("error", err.Error()).Error("panel returned an error when notifying it of a successful archive status")
			return
		}

		hasError = false

		// This log may not be displayed by the client due to the status event being sent before or at the same time.
		sendTransferLog("Successfully notified panel of successful archive status")

		l.Info("successfully notified panel of successful transfer archive status")
		s.Events().Publish(server.TransferStatusEvent, "archived")
	}(s)

	c.Status(http.StatusAccepted)
}

// Number of ticks in the progress bar
const ticks = 25

// 100% / number of ticks = percentage represented by each tick
const tickPercentage = 100 / ticks

type downloadProgress struct {
	size     uint64
	progress uint64
}

func (w *downloadProgress) Write(v []byte) (int, error) {
	n := len(v)

	atomic.AddUint64(&w.progress, uint64(n))

	return n, nil
}

func formatBytes(b uint64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}

	div, exp := int64(1024), 0
	for n := b / 1024; n >= 1024; n /= 1024 {
		div *= 1024
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func postTransfer(c *gin.Context) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(c.Request.Body); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	go func(data []byte) {
		serverID, _ := jsonparser.GetString(data, "server_id")
		url, _ := jsonparser.GetString(data, "url")
		token, _ := jsonparser.GetString(data, "token")

		l := log.WithField("server", serverID)
		l.Info("incoming transfer for server")

		// Create an http client with no timeout.
		client := &http.Client{Timeout: 0}

		hasError := true
		defer func() {
			if !hasError {
				return
			}

			l.Info("server transfer failed, notifying panel")
			if err := api.New().SendTransferFailure(serverID); err != nil {
				if !api.IsRequestError(err) {
					l.WithField("error", err).Error("failed to notify panel with transfer failure")
					return
				}

				l.WithField("error", err.Error()).Error("received error response from panel while notifying of transfer failure")
				return
			}

			l.Debug("notified panel of transfer failure")
		}()

		// Get the server data from the request.
		serverData, t, _, _ := jsonparser.Get(data, "server")
		if t != jsonparser.Object {
			l.Error("invalid server data passed in request")
			return
		}

		// Create a new server installer (note this does not execute the install script)
		i, err := installer.New(serverData)
		if err != nil {
			l.WithField("error", err).Error("failed to validate received server data")
			return
		}

		// Mark the server as transferring to prevent problems.
		i.Server().SetTransferring(true)

		// Add the server to the collection.
		server.GetServers().Add(i.Server())
		defer func() {
			if !hasError {
				return
			}

			// Remove the server if the transfer has failed.
			server.GetServers().Remove(func(s *server.Server) bool {
				return i.Server().Id() == s.Id()
			})
		}()

		// This function automatically adds the Target Node prefix and Timestamp to the log output before sending it
		// over the websocket.
		sendTransferLog := func(data string) {
			i.Server().Events().Publish(
				server.TransferLogsEvent,
				"\x1b[0;90m"+time.Now().Format(time.RFC1123)+"\x1b[0m \x1b[1;33m[Target Node]:\x1b[0m "+data,
			)
		}
		defer func() {
			if !hasError {
				return
			}

			i.Server().Events().Publish(server.TransferStatusEvent, "failure")
		}()

		sendTransferLog("Received incoming transfer from Panel, attempting to download archive from source node..")

		// Make a new GET request to the URL the panel gave us.
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			sendTransferLog("Failed to create http request: " + err.Error())
			log.WithField("error", err).Error("failed to create http request for archive transfer")
			return
		}

		// Add the authorization header on the request.
		req.Header.Set("Authorization", token)

		sendTransferLog("Requesting archive from source node..")
		l.Info("requesting archive for server transfer..")

		// Execute the http request.
		res, err := client.Do(req)
		if err != nil {
			sendTransferLog("Failed to send get archive request: " + err.Error())
			l.WithField("error", err).Error("failed to send archive http request")
			return
		}
		defer res.Body.Close()

		// Handle non-200 status codes.
		if res.StatusCode != 200 {
			sendTransferLog("Expected 200 but received \"" + strconv.Itoa(res.StatusCode) + "\" from source node while requesting archive")

			if _, err := ioutil.ReadAll(res.Body); err != nil {
				l.WithField("error", err).WithField("status", res.StatusCode).Error("failed read transfer response body")
				return
			}

			l.WithField("error", err).WithField("status", res.StatusCode).Error("failed to request server archive")
			return
		}

		size, err := strconv.ParseUint(res.Header.Get("Content-Length"), 10, 64)
		if err != nil {
			sendTransferLog("Failed to parse 'Content-Length' header: " + err.Error())
			l.WithField("error", err).Warn("failed to parse 'Content-Length' header")
			return
		}

		// Get the path to the archive.
		archivePath := filepath.Join(config.Get().System.ArchiveDirectory, serverID+".tar.gz")

		// Check if the archive already exists and delete it if it does.
		if _, err := os.Stat(archivePath); err != nil {
			if !os.IsNotExist(err) {
				sendTransferLog("Failed to stat archive file: " + err.Error())
				l.WithField("error", err).Error("failed to stat archive file")
				return
			}
		} else if err := os.Remove(archivePath); err != nil {
			sendTransferLog("Failed to remove old archive file: " + err.Error())
			l.WithField("error", err).Warn("failed to remove old archive file")
			return
		}

		// Create the file.
		file, err := os.Create(archivePath)
		if err != nil {
			sendTransferLog("Failed to open archive: " + err.Error())
			l.WithField("error", err).Error("failed to open archive on disk")
			return
		}

		sendTransferLog("Starting to write archive to disk..")
		l.Info("writing transfer archive to disk..")

		// Copy the file.
		progress := &downloadProgress{size: size}
		ticker := time.NewTicker(3 * time.Second)

		go func(progress *downloadProgress, t *time.Ticker) {
			for range ticker.C {
				// p = 100 (Downloaded)
				// size = 1000 (Content-Length)
				// p / size = 0.1
				// * 100 = 10% (Multiply by 100 to get a percentage of the download)
				// 10% / tickPercentage = (10% / (100 / 25)) (Divide by tick percentage to get the number of ticks)
				// 2.5 (Number of ticks as a float64)
				// 2 (convert to an integer)

				p := atomic.LoadUint64(&progress.progress)

				// We have to cast these numbers to float in order to get a float result from the division.
				width := float64(p) / float64(size)
				width *= 100
				width /= tickPercentage

				bar := strings.Repeat("=", int(width)) + strings.Repeat(" ", ticks-int(width))
				sendTransferLog("Downloading [" + bar + "] " + formatBytes(p) + " / " + formatBytes(progress.size))
			}
		}(progress, ticker)

		var reader io.Reader
		if downloadLimit := config.Get().System.Transfers.DownloadLimit; downloadLimit < 1 {
			// If there is no write limit, use the file as the writer.
			reader = res.Body
		} else {
			// Token bucket with a capacity of "downloadLimit" MiB, adding "downloadLimit" MiB/s
			bucket := ratelimit.NewBucketWithRate(float64(downloadLimit)*1024*1024, int64(downloadLimit)*1024*1024)

			// Wrap the file writer with the token bucket limiter.
			reader = ratelimit.Reader(res.Body, bucket)
		}

		buf := make([]byte, 1024*4)
		if _, err := io.CopyBuffer(file, io.TeeReader(reader, progress), buf); err != nil {
			sendTransferLog("Failed to write archive file to disk: " + err.Error())
			l.WithField("error", err).Error("failed to copy archive file to disk")
			return
		}
		ticker.Stop()

		// Show 100% completion.
		humanSize := formatBytes(progress.size)
		sendTransferLog("Downloading [" + strings.Repeat("=", ticks) + "] " + humanSize + " / " + humanSize)

		// Close the file so it can be opened to verify the checksum.
		if err := file.Close(); err != nil {
			sendTransferLog("Failed to close archive file: " + err.Error())
			l.WithField("error", err).Error("failed to close archive file")
			return
		}
		sendTransferLog("Successfully wrote archive to disk")
		l.Info("finished writing transfer archive to disk")

		// Whenever the transfer fails or succeeds, delete the temporary transfer archive.
		defer func() {
			log.WithField("server", serverID).Debug("deleting temporary transfer archive..")
			if err := os.Remove(archivePath); err != nil && !os.IsNotExist(err) {
				l.WithField("error", err).Warn("failed to delete transfer archive")
			} else {
				l.Debug("deleted temporary transfer archive successfully")
			}
		}()

		sendTransferLog("Successfully downloaded archive, computing checksum..")
		l.Info("server transfer archive downloaded, computing checksum...")

		// Open the archive file for computing a checksum.
		file, err = os.Open(archivePath)
		if err != nil {
			sendTransferLog("Failed to open archive file: " + err.Error())
			l.WithField("error", err).Error("failed to open archive on disk")
			return
		}

		// Compute the sha256 checksum of the file.
		hash := sha256.New()
		buf = make([]byte, 1024*4)
		if _, err := io.CopyBuffer(hash, file, buf); err != nil {
			sendTransferLog("Failed to copy archive file for checksum compute: " + err.Error())
			l.WithField("error", err).Error("failed to copy archive file for checksum computation")
			return
		}

		// Close the file.
		if err := file.Close(); err != nil {
			sendTransferLog("Failed to close archive: " + err.Error())
			l.WithField("error", err).Error("failed to close archive file after calculating checksum")
			return
		}

		sourceChecksum := res.Header.Get("X-Checksum")
		checksum := hex.EncodeToString(hash.Sum(nil))

		sendTransferLog("Successfully computed checksum")
		sendTransferLog("  -   Source Checksum: " + sourceChecksum)
		sendTransferLog("  - Computed Checksum: " + checksum)

		l.WithField("checksum", checksum).Info("computed checksum of transfer archive")

		// Verify the two checksums.
		if checksum != sourceChecksum {
			sendTransferLog("Checksum verification failed, aborting..")
			l.WithField("source_checksum", sourceChecksum).Error("checksum verification failed for archive")
			return
		}

		sendTransferLog("Archive checksum has been validated, continuing with transfer")
		l.Info("server archive transfer checksums have been validated, creating server environment..")

		// Create the server's environment.
		sendTransferLog("Creating server environment, this could take a while..")
		if err := i.Server().CreateEnvironment(); err != nil {
			sendTransferLog("Failed to create server environment: " + err.Error())
			l.WithField("error", err).Error("failed to create server environment")
			return
		}

		sendTransferLog("Server environment has been created, extracting transfer archive..")
		l.Info("server environment configured, extracting transfer archive..")
		// Extract the transfer archive.
		if err := archiver.NewTarGz().Unarchive(archivePath, i.Server().Filesystem().Path()); err != nil {
			// Unarchiving failed, delete the server's data directory.
			if err := os.RemoveAll(i.Server().Filesystem().Path()); err != nil && !os.IsNotExist(err) {
				sendTransferLog("Failed to delete server filesystem: " + err.Error())
				l.WithField("error", err).Warn("failed to delete server filesystem")
			} else {
				l.Debug("deleted server filesystem due to failed transfer")
			}

			sendTransferLog("Failed to extract archive: " + err.Error())
			l.WithField("error", err).Error("failed to extract server archive")
			return
		}

		// We mark the process as being successful here as if we fail to send a transfer success,
		// then a transfer failure won't probably be successful either.
		//
		// It may be useful to retry sending the transfer success every so often just in case of a small
		// hiccup or the fix of whatever error causing the success request to fail.
		hasError = false

		sendTransferLog("Archive has been extracted, attempting to notify panel..")
		l.Info("server transfer archive has been extracted, notifying panel..")

		// Notify the panel that the transfer succeeded.
		err = api.New().SendTransferSuccess(serverID)
		if err != nil {
			if !api.IsRequestError(err) {
				sendTransferLog("Failed to notify panel of transfer success: " + err.Error())
				l.WithField("error", err).Error("failed to notify panel of transfer success")
				return
			}

			sendTransferLog("Panel returned an error while notifying it of transfer success: " + err.Error())
			l.WithField("error", err.Error()).Error("panel responded with error after transfer success")
			return
		}

		i.Server().SetTransferring(false)

		sendTransferLog("Successfully notified panel of transfer success")
		l.Info("successfully notified panel of transfer success")

		i.Server().Events().Publish(server.TransferStatusEvent, "success")
		sendTransferLog("Transfer completed")
	}(buf.Bytes())

	c.Status(http.StatusAccepted)
}
