// Copyright (C) 2019 Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package httpd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/rs/xid"

	"github.com/drakkan/sftpgo/v2/internal/common"
)

const (
	pasteRootPath    = "/paste"
	pasteIndexPath   = "/paste/.index.json"
	pasteTextDir     = "/paste/text"
	pasteImagesDir   = "/paste/images"
	pasteMaxTextSize = 2 * 1024 * 1024
)

type pasteItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Content   string `json:"content,omitempty"`
	Path      string `json:"path,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

type pasteIndex struct {
	Items []pasteItem `json:"items"`
}

type textPasteRequest struct {
	Content string `json:"content"`
	Name    string `json:"name"`
}

func getPasteItems(w http.ResponseWriter, r *http.Request) {
	connection, err := getUserConnection(w, r)
	if err != nil {
		return
	}
	defer commonConnectionsRemove(connection)

	index, err := readPasteIndex(connection)
	if err != nil {
		sendAPIResponse(w, r, err, "Unable to read paste items", getMappedStatusCode(err))
		return
	}
	render.JSON(w, r, index.Items)
}

func createTextPaste(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, pasteMaxTextSize)
	connection, err := getUserConnection(w, r)
	if err != nil {
		return
	}
	defer commonConnectionsRemove(connection)

	var req textPasteRequest
	if err := render.DecodeJSON(r.Body, &req); err != nil {
		sendAPIResponse(w, r, err, "Invalid paste text payload", http.StatusBadRequest)
		return
	}
	req.Content = strings.TrimRight(req.Content, "\x00")
	if strings.TrimSpace(req.Content) == "" {
		sendAPIResponse(w, r, errors.New("paste content is required"), "", http.StatusBadRequest)
		return
	}

	if err := ensurePasteDirs(connection, pasteTextDir); err != nil {
		sendAPIResponse(w, r, err, "Unable to create paste directory", getMappedStatusCode(err))
		return
	}

	item := pasteItem{
		ID:        xid.New().String(),
		Type:      "text",
		Name:      cleanPasteName(req.Name, "Text paste"),
		Content:   req.Content,
		CreatedAt: time.Now().UTC().UnixMilli(),
	}
	item.Path = path.Join(pasteTextDir, item.ID+".json")

	if err := writePasteJSON(connection, item.Path, item); err != nil {
		sendAPIResponse(w, r, err, "Unable to save paste text", getMappedStatusCode(err))
		return
	}
	if err := addPasteIndexItem(connection, item); err != nil {
		sendAPIResponse(w, r, err, "Unable to update paste index", getMappedStatusCode(err))
		return
	}
	render.Status(r, http.StatusCreated)
	render.JSON(w, r, item)
}

func createImagePaste(w http.ResponseWriter, r *http.Request) {
	if maxUploadFileSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadFileSize)
	}
	connection, err := getUserConnection(w, r)
	if err != nil {
		return
	}
	defer commonConnectionsRemove(connection)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendAPIResponse(w, r, err, "Unable to read paste image", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		sendAPIResponse(w, r, errors.New("image content is required"), "", http.StatusBadRequest)
		return
	}
	mimeType := r.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(body)
	}
	ext, ok := pasteImageExtension(mimeType)
	if !ok {
		sendAPIResponse(w, r, fmt.Errorf("unsupported image content type %q", mimeType), "", http.StatusBadRequest)
		return
	}

	id := xid.New().String()
	dirPath := path.Join(pasteImagesDir, id)
	if err := ensurePasteDirs(connection, dirPath); err != nil {
		sendAPIResponse(w, r, err, "Unable to create paste image directory", getMappedStatusCode(err))
		return
	}

	item := pasteItem{
		ID:        id,
		Type:      "image",
		Name:      cleanPasteName(r.URL.Query().Get("name"), "Image paste"),
		Path:      path.Join(dirPath, "original"+ext),
		MimeType:  strings.Split(mimeType, ";")[0],
		Size:      int64(len(body)),
		CreatedAt: time.Now().UTC().UnixMilli(),
	}
	if err := writePasteBytes(connection, item.Path, body); err != nil {
		sendAPIResponse(w, r, err, "Unable to save paste image", getMappedStatusCode(err))
		return
	}
	if err := addPasteIndexItem(connection, item); err != nil {
		sendAPIResponse(w, r, err, "Unable to update paste index", getMappedStatusCode(err))
		return
	}
	render.Status(r, http.StatusCreated)
	render.JSON(w, r, item)
}

func deletePasteItem(w http.ResponseWriter, r *http.Request) {
	connection, err := getUserConnection(w, r)
	if err != nil {
		return
	}
	defer commonConnectionsRemove(connection)

	id := chi.URLParam(r, "id")
	index, err := readPasteIndex(connection)
	if err != nil {
		sendAPIResponse(w, r, err, "Unable to read paste items", getMappedStatusCode(err))
		return
	}
	itemIdx := slices.IndexFunc(index.Items, func(item pasteItem) bool {
		return item.ID == id
	})
	if itemIdx < 0 {
		sendAPIResponse(w, r, nil, "Paste item not found", http.StatusNotFound)
		return
	}

	item := index.Items[itemIdx]
	target := item.Path
	if item.Type == "image" {
		target = path.Dir(item.Path)
	}
	if target != "" {
		if err := connection.RemoveAll(target); err != nil {
			sendAPIResponse(w, r, err, "Unable to delete paste item", getMappedStatusCode(err))
			return
		}
	}
	index.Items = slices.Delete(index.Items, itemIdx, itemIdx+1)
	if err := writePasteIndex(connection, index); err != nil {
		sendAPIResponse(w, r, err, "Unable to update paste index", getMappedStatusCode(err))
		return
	}
	sendAPIResponse(w, r, nil, "Paste item deleted", http.StatusOK)
}

func readPasteIndex(connection *Connection) (pasteIndex, error) {
	var index pasteIndex
	fs, fsPath, err := connection.GetFsAndResolvedPath(pasteIndexPath)
	if err != nil {
		return index, err
	}
	if _, err := fs.Lstat(fsPath); err != nil {
		if fs.IsNotExist(err) {
			return index, nil
		}
		return index, connection.GetFsError(fs, err)
	}
	reader, err := connection.getFileReader(pasteIndexPath, 0, http.MethodGet)
	if err != nil {
		return index, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return index, err
	}
	if len(data) == 0 {
		return index, nil
	}
	err = json.Unmarshal(data, &index)
	return index, err
}

func addPasteIndexItem(connection *Connection, item pasteItem) error {
	index, err := readPasteIndex(connection)
	if err != nil {
		return err
	}
	index.Items = append([]pasteItem{item}, index.Items...)
	return writePasteIndex(connection, index)
}

func writePasteIndex(connection *Connection, index pasteIndex) error {
	if err := ensurePasteDirs(connection, pasteRootPath); err != nil {
		return err
	}
	return writePasteJSON(connection, pasteIndexPath, index)
}

func writePasteJSON(connection *Connection, filePath string, data any) error {
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	return writePasteBytes(connection, filePath, content)
}

func writePasteBytes(connection *Connection, filePath string, data []byte) error {
	connection.User.CheckFsRoot(connection.ID) //nolint:errcheck
	if err := connection.CheckParentDirs(path.Dir(filePath)); err != nil {
		return err
	}
	writer, err := connection.getFileWriter(filePath)
	if err != nil {
		return err
	}
	if _, err := writer.Write(data); err != nil {
		writer.Close() //nolint:errcheck
		return err
	}
	return writer.Close()
}

func ensurePasteDirs(connection *Connection, target string) error {
	connection.User.CheckFsRoot(connection.ID) //nolint:errcheck
	cleaned := connection.User.GetCleanedPath(target)
	current := "/"
	for _, part := range strings.Split(strings.Trim(cleaned, "/"), "/") {
		if part == "" {
			continue
		}
		current = path.Join(current, part)
		fs, fsPath, err := connection.GetFsAndResolvedPath(current)
		if err != nil {
			return err
		}
		if info, err := fs.Lstat(fsPath); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", current)
			}
			continue
		} else if !fs.IsNotExist(err) {
			return connection.GetFsError(fs, err)
		}
		if err := connection.CreateDir(current, true); err != nil {
			return err
		}
	}
	return nil
}

func pasteImageExtension(mimeType string) (string, bool) {
	switch strings.ToLower(strings.Split(mimeType, ";")[0]) {
	case "image/png":
		return ".png", true
	case "image/jpeg", "image/jpg":
		return ".jpg", true
	case "image/gif":
		return ".gif", true
	case "image/webp":
		return ".webp", true
	default:
		return "", false
	}
}

func cleanPasteName(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if len(value) > 120 {
		return value[:120]
	}
	return value
}

func commonConnectionsRemove(connection *Connection) {
	if connection != nil {
		common.Connections.Remove(connection.GetID())
	}
}
