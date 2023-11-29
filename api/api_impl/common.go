package api_impl

import (
	"github.com/jiaozifs/jiaozifs/api"
	"github.com/jiaozifs/jiaozifs/version"
	"go.uber.org/fx"
	"net/http"
)

var _ api.ServerInterface = (*APIController)(nil)

type APIController struct {
	fx.In
}

func (A APIController) GetVersion(w *api.JiaozifsResponse, r *http.Request) {
	swagger, err := api.GetSwagger()
	if err != nil {
		w.RespError(err)
		return
	}

	w.RespJSON(api.VersionResult{
		ApiVersion: swagger.Info.Version,
		Version:    version.UserVersion(),
	})
}
