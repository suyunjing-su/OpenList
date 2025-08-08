package sharing

import (
	"context"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	"github.com/pkg/errors"
)

func link(ctx context.Context, sid, path string, args *LinkArgs) (*model.Sharing, driver.Driver, *model.Link, model.Obj, error) {
	sharing, err := op.GetSharingById(sid, args.SharingListArgs.Refresh)
	if err != nil {
		return nil, nil, nil, nil, errors.WithStack(errs.SharingNotFound)
	}
	if !sharing.Valid() {
		return sharing, nil, nil, nil, errors.WithStack(errs.InvalidSharing)
	}
	if !sharing.Verify(args.Pwd) {
		return sharing, nil, nil, nil, errors.WithStack(errs.WrongShareCode)
	}
	path = utils.FixAndCleanPath(path)
	if len(sharing.Files) == 1 || path != "/" {
		storage, actualPath, err := op.GetSharingActualPath(sharing, path)
		if err != nil {
			return nil, nil, nil, nil, errors.WithMessage(err, "failed get sharing link")
		}
		l, obj, err := op.Link(ctx, storage, actualPath, args.LinkArgs)
		if err != nil {
			return nil, nil, nil, nil, errors.WithMessage(err, "failed get sharing link")
		}
		if l.URL != "" && !strings.HasPrefix(l.URL, "http://") && !strings.HasPrefix(l.URL, "https://") {
			l.URL = common.GetApiUrl(ctx) + l.URL
		}
		return sharing, storage, l, obj, nil
	}
	return nil, nil, nil, nil, errors.New("cannot get sharing root link")
}
