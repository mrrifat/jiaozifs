package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"time"

	"github.com/jiaozifs/jiaozifs/utils/hash"

	logging "github.com/ipfs/go-log/v2"

	"github.com/go-openapi/swag"
	"github.com/jiaozifs/jiaozifs/auth"
	"github.com/jiaozifs/jiaozifs/models/filemode"
	"github.com/jiaozifs/jiaozifs/versionmgr"

	"github.com/jiaozifs/jiaozifs/block"

	"github.com/jiaozifs/jiaozifs/utils"
	"github.com/jiaozifs/jiaozifs/utils/httputil"

	"github.com/jiaozifs/jiaozifs/models"

	"github.com/jiaozifs/jiaozifs/api"
	"go.uber.org/fx"
)

var objLog = logging.Logger("object_ctl")

type ObjectController struct {
	fx.In

	BlockAdapter block.Adapter

	Repo models.IRepo
}

func (oct ObjectController) DeleteObject(ctx context.Context, w *api.JiaozifsResponse, r *http.Request, ownerName string, repositoryName string, params api.DeleteObjectParams) { //nolint
	operator, err := auth.GetOperator(ctx)
	if err != nil {
		w.Error(err)
		return
	}

	owner, err := oct.Repo.UserRepo().Get(ctx, models.NewGetUserParams().SetName(ownerName))
	if err != nil {
		w.Error(err)
		return
	}

	if operator.Name != ownerName { //todo check permission
		w.Forbidden()
		return
	}

	repository, err := oct.Repo.RepositoryRepo().Get(ctx, models.NewGetRepoParams().SetOwnerID(owner.ID).SetName(repositoryName))
	if err != nil {
		w.Error(err)
		return
	}

	ref, err := oct.Repo.RefRepo().Get(ctx, models.NewGetRefParams().SetRepositoryID(repository.ID).SetName(params.Branch))
	if err != nil {
		w.Error(err)
		return
	}

	wip, err := oct.Repo.WipRepo().Get(ctx, models.NewGetWipParams().SetCreatorID(operator.ID).SetRepositoryID(repository.ID).SetRefID(ref.ID))
	if err != nil {
		w.Error(err)
		return
	}

	treeHash := hash.EmptyHash
	if !wip.CurrentTree.IsEmpty() {
		treeHash = wip.CurrentTree
	}

	workTree, err := versionmgr.NewWorkTree(ctx, oct.Repo.FileTreeRepo(repository.ID), models.NewRootTreeEntry(treeHash))
	if err != nil {
		w.Error(err)
		return
	}

	err = workTree.RemoveEntry(ctx, params.Path)
	if errors.Is(err, versionmgr.ErrPathNotFound) {
		w.BadRequest(fmt.Sprintf("path %s not found", params.Path))
		return
	}

	err = oct.Repo.WipRepo().UpdateByID(ctx, models.NewUpdateWipParams(wip.ID).SetCurrentTree(workTree.Root().Hash()))
	if err != nil {
		w.Error(err)
		return
	}
	w.OK()
}

func (oct ObjectController) GetObject(ctx context.Context, w *api.JiaozifsResponse, r *http.Request, ownerName string, repositoryName string, params api.GetObjectParams) { //nolint
	operator, err := auth.GetOperator(ctx)
	if err != nil {
		w.Error(err)
		return
	}

	owner, err := oct.Repo.UserRepo().Get(ctx, models.NewGetUserParams().SetName(ownerName))
	if err != nil {
		w.Error(err)
		return
	}

	if operator.Name != ownerName { //todo check permission
		w.Forbidden()
		return
	}

	repository, err := oct.Repo.RepositoryRepo().Get(ctx, models.NewGetRepoParams().SetOwnerID(owner.ID).SetName(repositoryName))
	if err != nil {
		w.Error(err)
		return
	}

	ref, err := oct.Repo.RefRepo().Get(ctx, models.NewGetRefParams().SetRepositoryID(repository.ID).SetName(params.Branch))
	if err != nil {
		w.Error(err)
		return
	}

	treeHash := hash.EmptyHash
	if utils.BoolValue(params.IsWip) {
		wip, err := oct.Repo.WipRepo().Get(ctx, models.NewGetWipParams().SetCreatorID(operator.ID).SetRepositoryID(repository.ID).SetRefID(ref.ID))
		if err != nil {
			w.Error(err)
			return
		}
		treeHash = wip.CurrentTree
	} else {

		if !ref.CommitHash.IsEmpty() {
			commit, err := oct.Repo.CommitRepo(repository.ID).Commit(ctx, ref.CommitHash)
			if err != nil {
				w.Error(err)
				return
			}
			treeHash = commit.TreeHash
		}
	}

	workTree, err := versionmgr.NewWorkTree(ctx, oct.Repo.FileTreeRepo(repository.ID), models.NewRootTreeEntry(treeHash))
	if err != nil {
		w.Error(err)
		return
	}

	blob, name, err := workTree.FindBlob(ctx, params.Path)
	if err != nil {
		if errors.Is(err, versionmgr.ErrPathNotFound) {
			w.BadRequest(fmt.Sprintf("path %s not found", params.Path))
			return
		}
		w.Error(err)
		return
	}
	reader, err := workTree.ReadBlob(ctx, oct.BlockAdapter, blob, params.Range)
	if err != nil {
		w.Error(err)
		return
	}
	defer reader.Close() //nolint
	// handle partial response if byte range supplied
	if params.Range != nil {
		rng, err := httputil.ParseRange(*params.Range, blob.Size)
		if err != nil {
			w.String("Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.StartOffset, rng.EndOffset, blob.Size))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rng.EndOffset-rng.StartOffset+1))
		w.Code(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", fmt.Sprint(blob.Size))
	}

	etag := httputil.ETag(blob.CheckSum.Hex())
	w.Header().Set("ETag", etag)
	lastModified := httputil.HeaderTimestamp(blob.CreatedAt)
	w.Header().Set("Last-Modified", lastModified)
	w.Header().Set("Content-Type", httputil.ExtensionsByType(name))
	// for security, make sure the browser and any proxies en route don't cache the response
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Expires", "0")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	_, err = io.Copy(w, reader)
	if err != nil {
		objLog.With(
			"user", ownerName,
			"repo", repositoryName,
			"path", params.Path).
			Debugf("GetObject copy content %v", err)

	}
}

func (oct ObjectController) HeadObject(ctx context.Context, w *api.JiaozifsResponse, r *http.Request, ownerName string, repositoryName string, params api.HeadObjectParams) { //nolint
	operator, err := auth.GetOperator(ctx)
	if err != nil {
		w.Error(err)
		return
	}

	owner, err := oct.Repo.UserRepo().Get(ctx, models.NewGetUserParams().SetName(ownerName))
	if err != nil {
		w.Error(err)
		return
	}

	if operator.Name != ownerName { //todo check permission
		w.Forbidden()
		return
	}

	repository, err := oct.Repo.RepositoryRepo().Get(ctx, models.NewGetRepoParams().SetOwnerID(owner.ID).SetName(repositoryName))
	if err != nil {
		w.Error(err)
		return
	}
	ref, err := oct.Repo.RefRepo().Get(ctx, models.NewGetRefParams().SetRepositoryID(repository.ID).SetName(params.Branch))
	if err != nil {
		w.Error(err)
		return
	}

	treeHash := hash.EmptyHash
	if utils.BoolValue(params.IsWip) {
		wip, err := oct.Repo.WipRepo().Get(ctx, models.NewGetWipParams().SetCreatorID(operator.ID).SetRepositoryID(repository.ID).SetRefID(ref.ID))
		if err != nil {
			w.Error(err)
			return
		}
		treeHash = wip.CurrentTree
	} else {

		if !ref.CommitHash.IsEmpty() {
			commit, err := oct.Repo.CommitRepo(repository.ID).Commit(ctx, ref.CommitHash)
			if err != nil {
				w.Error(err)
				return
			}
			treeHash = commit.TreeHash
		}
	}

	fileRepo := oct.Repo.FileTreeRepo(repository.ID)
	workTree, err := versionmgr.NewWorkTree(ctx, fileRepo, models.NewRootTreeEntry(treeHash))
	if err != nil {
		w.Error(err)
		return
	}

	blob, name, err := workTree.FindBlob(ctx, params.Path)
	if err != nil {
		if errors.Is(err, versionmgr.ErrPathNotFound) {
			w.BadRequest(fmt.Sprintf("path %s not found", params.Path))
			return
		}
		w.Error(err)
		return
	}

	//lookup files
	etag := httputil.ETag(blob.CheckSum.Hex())
	w.Header().Set("ETag", etag)
	lastModified := httputil.HeaderTimestamp(blob.CreatedAt)
	w.Header().Set("Last-Modified", lastModified)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", httputil.ExtensionsByType(name))
	// for security, make sure the browser and any proxies en route don't cache the response
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Expires", "0")

	// calculate possible byte range, if any.
	if params.Range != nil {
		rng, err := httputil.ParseRange(*params.Range, blob.Size)
		if err != nil {
			w.String(fmt.Sprintf("get blob range fail %v", err), http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.StartOffset, rng.EndOffset, blob.Size))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", rng.EndOffset-rng.StartOffset+1))
		w.Code(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", fmt.Sprint(blob.Size))
	}
}

func (oct ObjectController) UploadObject(ctx context.Context, w *api.JiaozifsResponse, r *http.Request, ownerName string, repositoryName string, params api.UploadObjectParams) { //nolint
	// read request body parse multipart for "content" and upload the data
	contentType := r.Header.Get("Content-Type")
	mediaType, p, err := mime.ParseMediaType(contentType)
	if err != nil {
		w.Error(err)
		return
	}

	reader := r.Body
	if mediaType == "multipart/form-data" {
		// handle multipart upload
		boundary, ok := p["boundary"]
		if !ok {
			w.Error(err)
			return
		}

		contentUploaded := false
		partReader := multipart.NewReader(r.Body, boundary)
		for !contentUploaded {
			part, err := partReader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.Error(err)
				return
			}
			contentType = part.Header.Get("Content-Type")
			partName := part.FormName()
			if partName == "content" {
				reader = part
				contentUploaded = true
			} else { //close not target part
				_ = part.Close()
			}

		}
		if !contentUploaded {
			w.Error(fmt.Errorf("multipart upload missing key 'content': %w", http.ErrMissingFile))
			return
		}
	}
	defer reader.Close() //nolint

	operator, err := auth.GetOperator(ctx)
	if err != nil {
		w.Error(err)
		return
	}

	owner, err := oct.Repo.UserRepo().Get(ctx, models.NewGetUserParams().SetName(ownerName))
	if err != nil {
		w.Error(err)
		return
	}

	if operator.Name != ownerName { //todo check permission
		w.Forbidden()
		return
	}

	repository, err := oct.Repo.RepositoryRepo().Get(ctx, models.NewGetRepoParams().SetOwnerID(owner.ID).SetName(repositoryName))
	if err != nil {
		w.Error(err)
		return
	}

	ref, err := oct.Repo.RefRepo().Get(ctx, models.NewGetRefParams().SetName(params.Branch).SetRepositoryID(repository.ID))
	if err != nil {
		w.Error(err)
		return
	}

	wip, err := oct.Repo.WipRepo().Get(ctx, models.NewGetWipParams().SetCreatorID(operator.ID).SetRepositoryID(repository.ID).SetRefID(ref.ID))
	if err != nil {
		w.Error(err)
		return
	}

	stash, err := oct.Repo.WipRepo().Get(ctx, models.NewGetWipParams().SetID(wip.ID))
	if err != nil {
		w.Error(err)
		return
	}

	var response api.ObjectStats
	err = oct.Repo.Transaction(ctx, func(dRepo models.IRepo) error {
		workingTree, err := versionmgr.NewWorkTree(ctx, dRepo.FileTreeRepo(repository.ID), models.NewRootTreeEntry(stash.CurrentTree))
		if err != nil {
			return err
		}

		// todo move write blob out of transaction
		blob, err := workingTree.WriteBlob(ctx, oct.BlockAdapter, reader, r.ContentLength, models.DefaultLeafProperty())
		if err != nil {
			return err
		}

		err = workingTree.AddLeaf(ctx, params.Path, blob)
		if err != nil {
			return err
		}
		response = api.ObjectStats{
			Checksum:    blob.CheckSum.Hex(),
			Mtime:       time.Now().Unix(),
			Path:        params.Path,
			PathMode:    utils.Uint32(uint32(filemode.Regular)),
			SizeBytes:   swag.Int64(blob.Size),
			ContentType: &contentType,
			Metadata:    &api.ObjectUserMetadata{},
		}
		return dRepo.WipRepo().UpdateByID(ctx, models.NewUpdateWipParams(stash.ID).SetCurrentTree(workingTree.Root().Hash()))
	})

	if err != nil {
		w.Error(err)
		return
	}

	w.JSON(response, http.StatusCreated)
}
