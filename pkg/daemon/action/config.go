package action

import (
	"context"

	"github.com/spf13/pflag"

	"github.com/wencaiwulue/kubevpn/v2/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/v2/pkg/handler"
	"github.com/wencaiwulue/kubevpn/v2/pkg/util"
)

var CancelFunc = make(map[string]context.CancelFunc)

func (svr *Server) ConfigAdd(ctx context.Context, req *rpc.ConfigAddRequest) (*rpc.ConfigAddResponse, error) {
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return nil, err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	sshCtx, sshCancel := context.WithCancel(context.Background())
	var path string
	path, err = handler.SshJump(sshCtx, sshConf, flags, true)
	CancelFunc[path] = sshCancel
	if err != nil {
		return nil, err
	}

	return &rpc.ConfigAddResponse{ClusterID: path}, nil
}

func (svr *Server) ConfigRemove(ctx context.Context, req *rpc.ConfigRemoveRequest) (*rpc.ConfigRemoveResponse, error) {
	if cancel, ok := CancelFunc[req.ClusterID]; ok {
		cancel()
	}
	return &rpc.ConfigRemoveResponse{}, nil
}
