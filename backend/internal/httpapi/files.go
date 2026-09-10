package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/b2"
	"cloudapp/internal/store"
)

// Upload is three-phase because the bytes bypass this server entirely
// (decision D1): init reserves a row and mints B2 credentials, the browser
// talks to B2, complete verifies and commits. Spec §4.2.

// mimeAllowlist is spec §8.
//
// Note what this can and cannot do. The server never sees a byte, so it cannot
// verify content type at all — the sniffed ftyp brand that actually decides is
// read in the browser. What the server does enforce is the thing that costs
// money if wrong: the byte count, checked against B2 itself at completion.
var mimeAllowlist = map[string]string{
	"image/jpeg":        "image",
	"image/png":         "image",
	"image/webp":        "image",
	"image/gif":         "image",
	"image/heic":        "image",
	"image/heif":        "image",
	"image/x-adobe-dng": "image",
	"video/mp4":         "video",
	"video/quicktime":   "video", // iPhone video is .MOV, not .MP4
	"video/webm":        "video",
}

// ---------------------------------------------------------------- init

type initRequest struct {
	Filename string `json:"filename" binding:"required"`
	Size     int64  `json:"size" binding:"required"`
	MimeType string `json:"mime_type" binding:"required"`
	FolderID *int64 `json:"folder_id"`
	Sha1     string `json:"sha1"`
	TakenAt  int64  `json:"taken_at"`
}

func (s *Server) handleFileInit(c *gin.Context) {
	var req initRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "filename, size and mime_type are required.")
		return
	}

	kind, ok := mimeAllowlist[strings.ToLower(strings.TrimSpace(req.MimeType))]
	if !ok {
		fail(c, http.StatusUnsupportedMediaType, "unsupported_type",
			"This app stores photos and videos. "+req.MimeType+" is not one.")
		return
	}
	if req.Size <= 0 || req.Size > store.MaxFileSize {
		fail(c, http.StatusRequestEntityTooLarge, "file_too_large",
			"Files must be between 1 byte and 500 MB.")
		return
	}

	user := currentUser(c)
	ctx := c.Request.Context()

	// Deduplication is free: B2 requires a SHA-1 on every upload anyway.
	if dup, err := s.Store.FindDuplicate(ctx, user.ID, req.Sha1); err == nil && dup != nil {
		c.JSON(http.StatusConflict, gin.H{
			"error": gin.H{"code": "duplicate", "message": "This file is already in your library."},
			"file":  gin.H{"id": dup.ID, "filename": dup.Filename},
		})
		return
	}

	file, err := s.Store.BeginUpload(ctx, store.NewFile{
		UserID:   user.ID,
		FolderID: req.FolderID,
		Filename: strings.TrimSpace(req.Filename),
		MimeType: req.MimeType,
		Kind:     kind,
		Size:     req.Size,
		Sha1:     req.Sha1,
		TakenAt:  req.TakenAt,
	})
	if errors.Is(err, store.ErrQuotaExceeded) {
		fail(c, http.StatusRequestEntityTooLarge, "quota_exceeded",
			"This upload would exceed your storage quota. Delete something first.")
		return
	}
	if err != nil {
		s.Log.Error("begin upload failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not start the upload. Try again.")
		return
	}

	origName := b2.OrigName(user.ID, file.ObjectKey, file.Ext)
	thumbName := b2.ThumbName(user.ID, file.ObjectKey)

	payload := gin.H{
		"file_id":    file.ID,
		"object_key": file.ObjectKey,
		"orig_name":  origName,
		"thumb_name": thumbName,
		"part_size":  b2.PartSize,
	}

	if req.Size > b2.LargeFileCutoff {
		largeFileID, err := s.B2.StartLargeUpload(ctx, origName, req.MimeType, req.Sha1)
		if err != nil {
			s.abandon(c, user.ID, file.ID)
			s.b2Fail(c, "start large upload", err)
			return
		}
		payload["large_file_id"] = largeFileID
		payload["multipart"] = true
	} else {
		target, err := s.B2.UploadCredentials(ctx, origName)
		if err != nil {
			s.abandon(c, user.ID, file.ID)
			s.b2Fail(c, "mint upload credentials", err)
			return
		}
		payload["orig"] = target
		payload["multipart"] = false
	}

	// The thumbnail is always a single small upload, whatever the original's
	// size, so it never needs the multipart path.
	if thumbTarget, err := s.B2.UploadCredentials(ctx, thumbName); err == nil {
		payload["thumb"] = thumbTarget
	} else {
		// Not fatal: the original still uploads and the row lands in the tier 3
		// queue (spec §5.4).
		s.Log.Warn("could not mint thumbnail upload credentials", "err", err)
	}

	c.JSON(http.StatusOK, payload)
}

// abandon drops a reserved row after a failure between init and upload, so a
// B2 outage does not leave the quota accounting counting phantom bytes.
func (s *Server) abandon(c *gin.Context, userID, fileID int64) {
	if err := s.Store.AbandonUpload(c.Request.Context(), userID, fileID); err != nil {
		s.Log.Warn("could not abandon reserved upload", "file_id", fileID, "err", err)
	}
}

// ---------------------------------------------------------------- init-parts

type initPartsRequest struct {
	FileID      int64  `json:"file_id" binding:"required"`
	LargeFileID string `json:"large_file_id" binding:"required"`
	Count       int    `json:"count"`
}

// handleFileInitParts mints one upload target per concurrent worker, because a
// B2 upload URL serves exactly one upload at a time.
func (s *Server) handleFileInitParts(c *gin.Context) {
	var req initPartsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "file_id and large_file_id are required.")
		return
	}

	user := currentUser(c)
	file, err := s.Store.FileByID(c.Request.Context(), user.ID, req.FileID)
	if err != nil || file.Status != "uploading" {
		fail(c, http.StatusNotFound, "not_found", "No upload in progress with that id.")
		return
	}

	count := req.Count
	if count <= 0 {
		count = 4
	}
	targets, err := s.B2.PartCredentials(c.Request.Context(), req.LargeFileID, count)
	if err != nil {
		s.b2Fail(c, "mint part credentials", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"urls": targets})
}

// ---------------------------------------------------------------- complete

type completeRequest struct {
	B2FileID    string   `json:"b2_file_id"`
	LargeFileID string   `json:"large_file_id"`
	PartSha1s   []string `json:"part_sha1s"`
	Sha1        string   `json:"sha1"`
	Width       *int     `json:"width"`
	Height      *int     `json:"height"`
	DurationSec *float64 `json:"duration_sec"`
	TakenAt     int64    `json:"taken_at"`
	ThumbOK     bool     `json:"thumb_ok"`
	ThumbTier   string   `json:"thumb_tier"`
	ThumbFormat string   `json:"thumb_format"`
}

func (s *Server) handleFileComplete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req completeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Malformed completion payload.")
		return
	}

	user := currentUser(c)
	ctx := c.Request.Context()

	file, err := s.Store.FileByID(ctx, user.ID, id)
	if errors.Is(err, store.ErrNoFile) {
		fail(c, http.StatusNotFound, "not_found", "No such upload.")
		return
	}
	if err != nil {
		s.Log.Error("load file failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Something went wrong. Try again.")
		return
	}
	if file.Status != "uploading" {
		fail(c, http.StatusConflict, "already_complete", "This upload was already finished.")
		return
	}

	b2FileID := req.B2FileID
	if req.LargeFileID != "" {
		info, err := s.B2.FinishLargeUpload(ctx, req.LargeFileID, req.PartSha1s)
		if err != nil {
			s.b2Fail(c, "finish large upload", err)
			return
		}
		b2FileID = info.FileID
	}
	if b2FileID == "" {
		fail(c, http.StatusBadRequest, "invalid_request", "No B2 file id was reported.")
		return
	}

	// The counterweight to letting the browser talk to B2 directly: confirm
	// with B2 itself that what landed matches what was declared. Without this a
	// client could declare 1 KB, store 4 GB, and walk through the quota.
	info, err := s.B2.Verify(ctx, b2FileID, file.FileSize, req.Sha1)
	if err != nil {
		if errors.Is(err, b2.ErrSizeMismatch) || errors.Is(err, b2.ErrDigestMismatch) {
			s.Log.Warn("upload verification failed", "file_id", file.ID, "err", err)
			// Do not keep an object we cannot account for.
			if delErr := s.B2.Delete(ctx, b2.OrigName(user.ID, file.ObjectKey, file.Ext), b2FileID); delErr != nil {
				s.Log.Error("could not remove unverifiable object", "err", delErr)
			}
			s.abandon(c, user.ID, file.ID)
			fail(c, http.StatusBadRequest, "verification_failed",
				"The uploaded file did not match what was declared. Nothing was saved.")
			return
		}
		s.b2Fail(c, "verify upload", err)
		return
	}

	saved, err := s.Store.CompleteUpload(ctx, user.ID, file.ID, store.CompleteFile{
		B2FileID:    b2FileID,
		Sha1:        info.WholeSha1(),
		Width:       req.Width,
		Height:      req.Height,
		DurationSec: req.DurationSec,
		TakenAt:     req.TakenAt,
		ThumbOK:     req.ThumbOK,
		ThumbTier:   req.ThumbTier,
		ThumbFormat: req.ThumbFormat,
	})
	if err != nil {
		s.Log.Error("complete upload failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not finish the upload.")
		return
	}

	s.writeSidecar(c, user.ID, saved)

	build, err := s.B2.URLBuilder(ctx, user.ID)
	if err != nil {
		s.b2Fail(c, "compose urls", err)
		return
	}
	c.JSON(http.StatusOK, filePayload(saved, build))
}

// writeSidecar mirrors the row's metadata into B2 next to the object.
//
// This is the second line of defence in spec §7.3: if the VPS and every
// database backup are lost, listing the meta/ prefix rebuilds the whole index.
// Best-effort — an upload that stored its bytes must not fail because the
// recovery copy did not write.
func (s *Server) writeSidecar(c *gin.Context, userID int64, f *store.File) {
	body, err := json.Marshal(map[string]any{
		"object_key":   f.ObjectKey,
		"filename":     f.Filename,
		"ext":          f.Ext,
		"mime_type":    f.MimeType,
		"kind":         f.Kind,
		"file_size":    f.FileSize,
		"sha1":         f.Sha1,
		"b2_file_id":   f.B2FileID,
		"width":        f.Width,
		"height":       f.Height,
		"duration_sec": f.DurationSec,
		"taken_at":     f.TakenAt,
		"uploaded_at":  f.UploadedAt,
		"folder_id":    f.FolderID,
	})
	if err != nil {
		s.Log.Error("marshal sidecar failed", "err", err)
		return
	}
	if err := s.B2.PutSidecar(c.Request.Context(), userID, f.ObjectKey, body); err != nil {
		s.Log.Warn("sidecar write failed; this file is not covered by index recovery",
			"object_key", f.ObjectKey, "err", err)
	}
}

// ---------------------------------------------------------------- abort

// handleFileAbort lets a client that gave up clean its own mess immediately,
// rather than leaving an unfinished large file for the orphan sweep to find a
// day later. B2 bills the parts of an unfinished upload the whole time.
func (s *Server) handleFileAbort(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	user := currentUser(c)
	ctx := c.Request.Context()

	if largeFileID := c.Query("large_file_id"); largeFileID != "" {
		if err := s.B2.CancelLargeUpload(ctx, largeFileID); err != nil {
			s.Log.Warn("cancel large file failed", "err", err)
		}
	}
	if err := s.Store.AbandonUpload(ctx, user.ID, id); err != nil && !errors.Is(err, store.ErrNoFile) {
		s.Log.Warn("abandon upload failed", "err", err)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------- list

func (s *Server) handleFileList(c *gin.Context) {
	user := currentUser(c)
	ctx := c.Request.Context()

	filter := store.ListFilter{
		Kind:    c.Query("kind"),
		Query:   c.Query("q"),
		Cursor:  c.Query("cursor"),
		Deleted: c.Query("deleted") == "1",
	}
	if v := c.Query("limit"); v != "" {
		filter.Limit, _ = strconv.Atoi(v)
	}
	if v, present := c.GetQuery("folder_id"); present {
		filter.InFolder = true
		if v != "" && v != "root" {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fail(c, http.StatusBadRequest, "invalid_request", "folder_id must be a number or \"root\".")
				return
			}
			filter.FolderID = &id
		}
	}
	if filter.Kind != "" && filter.Kind != "image" && filter.Kind != "video" {
		fail(c, http.StatusBadRequest, "invalid_request", "kind must be image or video.")
		return
	}

	result, err := s.Store.ListFiles(ctx, user.ID, filter)
	if errors.Is(err, store.ErrBadCursor) {
		fail(c, http.StatusBadRequest, "bad_cursor", "That page link is no longer valid.")
		return
	}
	if err != nil {
		s.Log.Error("list files failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not load your files.")
		return
	}

	// One prefix token serves every URL on the page (spec §2.4).
	build, err := s.B2.URLBuilder(ctx, user.ID)
	if err != nil {
		s.b2Fail(c, "compose urls", err)
		return
	}

	items := make([]gin.H, 0, len(result.Files))
	for _, f := range result.Files {
		items = append(items, filePayload(f, build))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": result.NextCursor})
}

func (s *Server) handleFileGet(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	user := currentUser(c)
	ctx := c.Request.Context()

	file, err := s.Store.FileByID(ctx, user.ID, id)
	if errors.Is(err, store.ErrNoFile) {
		// A file belonging to someone else is "not found", not "forbidden":
		// 403 would confirm the id exists.
		fail(c, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	if err != nil {
		s.Log.Error("load file failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Something went wrong.")
		return
	}

	build, err := s.B2.URLBuilder(ctx, user.ID)
	if err != nil {
		s.b2Fail(c, "compose urls", err)
		return
	}
	c.JSON(http.StatusOK, filePayload(file, build))
}

// ---------------------------------------------------------------- mutate

type patchFileRequest struct {
	Filename *string `json:"filename"`
	FolderID *int64  `json:"folder_id"`
	ToRoot   bool    `json:"to_root"`
}

func (s *Server) handleFilePatch(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req patchFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Nothing to change.")
		return
	}

	user := currentUser(c)
	ctx := c.Request.Context()

	if req.Filename != nil {
		if err := s.Store.RenameFile(ctx, user.ID, id, *req.Filename); err != nil {
			s.storeFail(c, err, "rename")
			return
		}
	}
	if req.FolderID != nil || req.ToRoot {
		target := req.FolderID
		if req.ToRoot {
			target = nil
		}
		if err := s.Store.MoveFile(ctx, user.ID, id, target); err != nil {
			s.storeFail(c, err, "move")
			return
		}
	}

	file, err := s.Store.FileByID(ctx, user.ID, id)
	if err != nil {
		s.storeFail(c, err, "reload")
		return
	}
	build, err := s.B2.URLBuilder(ctx, user.ID)
	if err != nil {
		s.b2Fail(c, "compose urls", err)
		return
	}
	c.JSON(http.StatusOK, filePayload(file, build))
}

func (s *Server) handleFileDelete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	user := currentUser(c)
	n, err := s.Store.SoftDelete(c.Request.Context(), user.ID, []int64{id})
	if err != nil {
		s.storeFail(c, err, "delete")
		return
	}
	if n == 0 {
		fail(c, http.StatusNotFound, "not_found", "No such file.")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": n, "recoverable_days": 30})
}

type idsRequest struct {
	IDs []int64 `json:"ids" binding:"required"`
}

func (s *Server) handleFileBulkDelete(c *gin.Context) {
	var req idsRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 {
		fail(c, http.StatusBadRequest, "invalid_request", "Select at least one file.")
		return
	}
	if len(req.IDs) > 500 {
		fail(c, http.StatusBadRequest, "too_many", "Delete at most 500 files at a time.")
		return
	}
	user := currentUser(c)
	n, err := s.Store.SoftDelete(c.Request.Context(), user.ID, req.IDs)
	if err != nil {
		s.storeFail(c, err, "bulk delete")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": n, "recoverable_days": 30})
}

func (s *Server) handleFileRestore(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	user := currentUser(c)
	n, err := s.Store.Restore(c.Request.Context(), user.ID, []int64{id})
	if err != nil {
		s.storeFail(c, err, "restore")
		return
	}
	if n == 0 {
		fail(c, http.StatusNotFound, "not_found", "No such file in the trash.")
		return
	}
	c.JSON(http.StatusOK, gin.H{"restored": n})
}

// ---------------------------------------------------------------- storage

func (s *Server) handleStorageUsage(c *gin.Context) {
	user := currentUser(c)
	stats, err := s.Store.StorageStats(c.Request.Context(), user.ID)
	if err != nil {
		s.Log.Error("storage stats failed", "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Could not read storage usage.")
		return
	}
	percent := 0.0
	if stats.Quota > 0 {
		percent = float64(stats.Used) / float64(stats.Quota) * 100
	}
	c.JSON(http.StatusOK, gin.H{
		"used_bytes":         stats.Used,
		"quota_bytes":        stats.Quota,
		"usage_percent":      percent,
		"image_bytes":        stats.Images,
		"video_bytes":        stats.Videos,
		"trash_bytes":        stats.Trash,
		"file_count":         stats.Count,
		"thumbnails_pending": stats.Pending,
	})
}

// ---------------------------------------------------------------- shared

func filePayload(f *store.File, url func(string) string) gin.H {
	payload := gin.H{
		"id":           f.ID,
		"filename":     f.Filename,
		"kind":         f.Kind,
		"mime_type":    f.MimeType,
		"file_size":    f.FileSize,
		"taken_at":     f.TakenAt,
		"uploaded_at":  f.UploadedAt,
		"thumb_status": f.ThumbStatus,
		"status":       f.Status,
		"orig_url":     url(b2.OrigName(f.UserID, f.ObjectKey, f.Ext)),
	}
	if f.HasThumb {
		payload["thumb_url"] = url(b2.ThumbName(f.UserID, f.ObjectKey))
	}
	if f.FolderID != nil {
		payload["folder_id"] = *f.FolderID
	}
	if f.Width != nil && f.Height != nil {
		payload["width"], payload["height"] = *f.Width, *f.Height
	}
	if f.DurationSec != nil {
		payload["duration_sec"] = *f.DurationSec
	}
	if f.IsDeleted {
		payload["is_deleted"] = true
		payload["deleted_at"] = f.DeletedAt
	}
	return payload
}

func pathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(c, http.StatusBadRequest, "invalid_request", "That is not a valid id.")
		return 0, false
	}
	return id, true
}

// b2Fail maps a storage failure to a response that says whether the user can
// do anything about it.
func (s *Server) b2Fail(c *gin.Context, action string, err error) {
	switch {
	case errors.Is(err, b2.ErrNotConfigured):
		s.Log.Error("b2 not configured", "action", action)
		fail(c, http.StatusServiceUnavailable, "storage_unavailable",
			"Storage is not configured on this server yet.")
	case errors.Is(err, b2.ErrNoUploadKey):
		// Refusing here is the point: the alternative is handing a browser a
		// master key (spec §2.3).
		s.Log.Error("no restricted upload key; refusing to mint browser credentials", "action", action)
		fail(c, http.StatusServiceUnavailable, "storage_unavailable",
			"Uploads are disabled until the restricted storage key is configured.")
	default:
		s.Log.Error("b2 call failed", "action", action, "err", err)
		fail(c, http.StatusBadGateway, "storage_error", "Storage is not responding. Try again shortly.")
	}
}

func (s *Server) storeFail(c *gin.Context, err error, action string) {
	switch {
	case errors.Is(err, store.ErrNoFile):
		fail(c, http.StatusNotFound, "not_found", "No such file.")
	case errors.Is(err, store.ErrNoFolder):
		fail(c, http.StatusNotFound, "not_found", "No such folder.")
	case errors.Is(err, store.ErrFolderExists):
		fail(c, http.StatusConflict, "folder_exists", "A folder with that name already exists.")
	case errors.Is(err, store.ErrFolderNotEmpty):
		fail(c, http.StatusConflict, "folder_not_empty",
			"That folder still has files in it. Move them out, or delete them along with it.")
	case errors.Is(err, store.ErrQuotaExceeded):
		fail(c, http.StatusRequestEntityTooLarge, "quota_exceeded", "That would exceed your storage quota.")
	case errors.Is(err, store.ErrWrongState):
		fail(c, http.StatusConflict, "wrong_state", err.Error())
	default:
		s.Log.Error("store operation failed", "action", action, "err", err)
		fail(c, http.StatusInternalServerError, "internal", "Something went wrong. Try again.")
	}
}
