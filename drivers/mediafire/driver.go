package mediafire

/*
Package mediafire
Author: Da3zKi7<da3zki7@duck.com>
Date: 2025-09-11

D@' 3z K!7 - The King Of Cracking

Modifications by ILoveScratch2<ilovescratch@foxmail.com>
Date: 2025-09-21
*/

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"golang.org/x/time/rate"
)

type Mediafire struct {
	model.Storage
	Addition

	actionToken string
	limiter     *rate.Limiter

	appBase    string
	apiBase    string
	hostBase   string
	maxRetries int

	secChUa         string
	secChUaPlatform string
	userAgent       string
}

func (d *Mediafire) Config() driver.Config {
	return config
}

func (d *Mediafire) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Mediafire) Init(ctx context.Context) error {
	if d.SessionToken == "" {
		return fmt.Errorf("Init :: [MediaFire] {critical} missing sessionToken")
	}

	if d.Cookie == "" {
		return fmt.Errorf("Init :: [MediaFire] {critical} missing Cookie")
	}
	if d.LimitRate > 0 {
		d.limiter = rate.NewLimiter(rate.Limit(d.LimitRate), 1)
	}
	if _, err := d.getSessionToken(ctx); err != nil {
		return d.renewToken(ctx)
	}

	return nil
}

func (d *Mediafire) WaitLimit(ctx context.Context) error {
	if d.limiter != nil {
		return d.limiter.Wait(ctx)
	}
	return nil
}

func (d *Mediafire) Drop(ctx context.Context) error {
	// Clear cached resources
	d.actionToken = ""
	return nil
}

func (d *Mediafire) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(ctx, dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return d.fileToObj(src), nil
	})
}

func (d *Mediafire) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {

	downloadUrl, err := d.getDirectDownloadLink(ctx, file.GetID())
	if err != nil {
		return nil, err
	}

	res, err := base.NoRedirectClient.R().SetDoNotParseResponse(true).SetContext(ctx).Head(downloadUrl)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = res.RawBody().Close()
	}()

	if res.StatusCode() == 302 {
		downloadUrl = res.Header().Get("location")
	}

	return &model.Link{
		URL: downloadUrl,
		Header: http.Header{
			"Origin":             []string{d.appBase},
			"Referer":            []string{d.appBase + "/"},
			"sec-ch-ua":          []string{d.secChUa},
			"sec-ch-ua-platform": []string{d.secChUaPlatform},
			"User-Agent":         []string{d.userAgent},
		},
	}, nil
}

func (d *Mediafire) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	data := map[string]string{
		"session_token":   d.SessionToken,
		"response_format": "json",
		"parent_key":      parentDir.GetID(),
		"foldername":      dirName,
	}

	var resp MediafireFolderCreateResponse
	_, err := d.postForm("/folder/create.php", data, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Response.Result != "Success" {
		return nil, fmt.Errorf("MediaFire API error: %s", resp.Response.Result)
	}

	created, _ := time.Parse("2006-01-02T15:04:05Z", resp.Response.CreatedUTC)

	return &model.Object{
		ID:       resp.Response.FolderKey,
		Name:     resp.Response.Name,
		Size:     0,
		Modified: created,
		Ctime:    created,
		IsFolder: true,
	}, nil
}

func (d *Mediafire) Move(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	var data map[string]string
	var endpoint string

	if srcObj.IsDir() {

		endpoint = "/folder/move.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"folder_key_src":  srcObj.GetID(),
			"folder_key_dst":  dstDir.GetID(),
		}
	} else {

		endpoint = "/file/move.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"quick_key":       srcObj.GetID(),
			"folder_key":      dstDir.GetID(),
		}
	}

	var resp MediafireMoveResponse
	_, err := d.postForm(endpoint, data, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Response.Result != "Success" {
		return nil, fmt.Errorf("MediaFire API error: %s", resp.Response.Result)
	}

	return srcObj, nil
}

func (d *Mediafire) Rename(ctx context.Context, srcObj model.Obj, newName string) (model.Obj, error) {
	var data map[string]string
	var endpoint string

	if srcObj.IsDir() {

		endpoint = "/folder/update.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"folder_key":      srcObj.GetID(),
			"foldername":      newName,
		}
	} else {

		endpoint = "/file/update.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"quick_key":       srcObj.GetID(),
			"filename":        newName,
		}
	}

	var resp MediafireRenameResponse
	_, err := d.postForm(endpoint, data, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Response.Result != "Success" {
		return nil, fmt.Errorf("MediaFire API error: %s", resp.Response.Result)
	}

	return &model.Object{
		ID:       srcObj.GetID(),
		Name:     newName,
		Size:     srcObj.GetSize(),
		Modified: srcObj.ModTime(),
		Ctime:    srcObj.CreateTime(),
		IsFolder: srcObj.IsDir(),
	}, nil
}

func (d *Mediafire) Copy(ctx context.Context, srcObj, dstDir model.Obj) (model.Obj, error) {
	var data map[string]string
	var endpoint string

	if srcObj.IsDir() {

		endpoint = "/folder/copy.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"folder_key_src":  srcObj.GetID(),
			"folder_key_dst":  dstDir.GetID(),
		}
	} else {

		endpoint = "/file/copy.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"quick_key":       srcObj.GetID(),
			"folder_key":      dstDir.GetID(),
		}
	}

	var resp MediafireCopyResponse
	_, err := d.postForm(endpoint, data, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Response.Result != "Success" {
		return nil, fmt.Errorf("MediaFire API error: %s", resp.Response.Result)
	}

	var newID string
	if srcObj.IsDir() {
		if len(resp.Response.NewFolderKeys) > 0 {
			newID = resp.Response.NewFolderKeys[0]
		}
	} else {
		if len(resp.Response.NewQuickKeys) > 0 {
			newID = resp.Response.NewQuickKeys[0]
		}
	}

	return &model.Object{
		ID:       newID,
		Name:     srcObj.GetName(),
		Size:     srcObj.GetSize(),
		Modified: srcObj.ModTime(),
		Ctime:    srcObj.CreateTime(),
		IsFolder: srcObj.IsDir(),
	}, nil
}

func (d *Mediafire) Remove(ctx context.Context, obj model.Obj) error {
	var data map[string]string
	var endpoint string

	if obj.IsDir() {

		endpoint = "/folder/delete.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"folder_key":      obj.GetID(),
		}
	} else {

		endpoint = "/file/delete.php"
		data = map[string]string{
			"session_token":   d.SessionToken,
			"response_format": "json",
			"quick_key":       obj.GetID(),
		}
	}

	var resp MediafireRemoveResponse
	_, err := d.postForm(endpoint, data, &resp)
	if err != nil {
		return err
	}

	if resp.Response.Result != "Success" {
		return fmt.Errorf("MediaFire API error: %s", resp.Response.Result)
	}

	return nil
}

func (d *Mediafire) Put(ctx context.Context, dstDir model.Obj, file model.FileStreamer, up driver.UpdateProgress) (model.Obj, error) {
	fileHash := file.GetHash().GetHash(utils.SHA256)
	var tempFile model.File
	var err error
	
	// Try to use existing hash first, cache only if necessary
	if len(fileHash) != utils.SHA256.Width {
		tempFile, fileHash, err = stream.CacheFullAndHash(file, &up, utils.SHA256)
		if err != nil {
			return nil, err
		}
	} else {
		tempFile = file.GetFile()
		if tempFile == nil {
			// Cache the file if hash exists but file not cached
			tempFile, _, err = stream.CacheFullAndHash(file, &up, utils.SHA256)
			if err != nil {
				return nil, err
			}
		}
	}

	checkResp, err := d.uploadCheck(ctx, file.GetName(), file.GetSize(), fileHash, dstDir.GetID())
	if err != nil {
		return nil, err
	}

	// Quick upload: file already exists in account
	if checkResp.Response.HashExists == "yes" && checkResp.Response.InAccount == "yes" {
		up(100.0)
		existingFile, err := d.getExistingFileInfo(ctx, fileHash, file.GetName(), dstDir.GetID())
		if err == nil {
			return &model.Object{
				ID:   existingFile.GetID(),
				Name: file.GetName(),
				Size: file.GetSize(),
			}, nil
		}
	}

	var pollKey string

	if checkResp.Response.ResumableUpload.AllUnitsReady != "yes" {
		pollKey, err = d.uploadUnits(ctx, tempFile, checkResp, file.GetName(), fileHash, dstDir.GetID(), up)
		if err != nil {
			return nil, err
		}
	} else {
		pollKey = checkResp.Response.ResumableUpload.UploadKey
		up(100.0)
	}

	pollResp, err := d.pollUpload(ctx, pollKey)
	if err != nil {
		return nil, err
	}

	return &model.Object{
		ID:   pollResp.Response.Doupload.QuickKey,
		Name: file.GetName(),
		Size: file.GetSize(),
	}, nil
}

var _ driver.Driver = (*Mediafire)(nil)
