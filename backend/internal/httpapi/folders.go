package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"cloudapp/internal/store"
)

func (s *Server) handleFolderList(c *gin.Context) {
	user := currentUser(c)
	folders, err := s.Store.ListFolders(c.Request.Context(), user.ID)
	if err != nil {
		s.storeFail(c, err, "list folders")
		return
	}
	items := make([]gin.H, 0, len(folders))
	for _, f := range folders {
		items = append(items, folderPayload(f))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

type folderRequest struct {
	Name string `json:"name" binding:"required"`
}

func (s *Server) handleFolderCreate(c *gin.Context) {
	var req folderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Give the folder a name.")
		return
	}
	user := currentUser(c)
	folder, err := s.Store.CreateFolder(c.Request.Context(), user.ID, req.Name)
	if err != nil {
		s.storeFail(c, err, "create folder")
		return
	}
	c.JSON(http.StatusCreated, folderPayload(folder))
}

func (s *Server) handleFolderRename(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req folderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid_request", "Give the folder a name.")
		return
	}
	user := currentUser(c)
	ctx := c.Request.Context()

	if err := s.Store.RenameFolder(ctx, user.ID, id, req.Name); err != nil {
		s.storeFail(c, err, "rename folder")
		return
	}
	folder, err := s.Store.FolderByID(ctx, user.ID, id)
	if err != nil {
		s.storeFail(c, err, "reload folder")
		return
	}
	c.JSON(http.StatusOK, folderPayload(folder))
}

// handleFolderDelete refuses to empty a folder by accident.
//
// Deleting a folder full of photos is not obviously the same request as
// deleting the folder, so the default is a 409 that names the choice. Cascade
// soft-deletes, which keeps the files recoverable for thirty days rather than
// destroying them.
func (s *Server) handleFolderDelete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}

	mode := store.DeleteRefuse
	switch {
	case c.Query("cascade") == "1":
		mode = store.DeleteCascade
	case c.Query("move_to") == "root":
		mode = store.DeleteMoveToRoot
	}

	user := currentUser(c)
	affected, err := s.Store.DeleteFolder(c.Request.Context(), user.ID, id, mode)
	if err != nil {
		s.storeFail(c, err, "delete folder")
		return
	}

	out := gin.H{"deleted": true}
	switch mode {
	case store.DeleteCascade:
		out["files_trashed"] = affected
		out["recoverable_days"] = 30
	case store.DeleteMoveToRoot:
		out["files_moved_to_root"] = affected
	}
	c.JSON(http.StatusOK, out)
}

func folderPayload(f *store.Folder) gin.H {
	return gin.H{
		"id":         f.ID,
		"name":       f.Name,
		"file_count": f.FileCount,
		"created_at": f.CreatedAt,
	}
}
