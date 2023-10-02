package action

import (
	"context"
	"fmt"
	"io"
	defaultlog "log"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/utils/pointer"

	"github.com/wencaiwulue/kubevpn/pkg/config"
	"github.com/wencaiwulue/kubevpn/pkg/daemon/rpc"
	"github.com/wencaiwulue/kubevpn/pkg/handler"
	"github.com/wencaiwulue/kubevpn/pkg/util"
)

func (svr *Server) Connect(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) error {
	defer func() {
		log.SetOutput(svr.LogFile)
		log.SetLevel(log.DebugLevel)
	}()
	out := io.MultiWriter(newWarp(resp), svr.LogFile)
	log.SetOutput(out)
	log.SetLevel(log.InfoLevel)
	if !svr.IsSudo {
		return svr.redirectToSudoDaemon(req, resp)
	}

	ctx := resp.Context()
	if !svr.t.IsZero() {
		log.Debugf("already connect to another cluster, you can disconnect this connect by command `kubevpn disconnect`")
		// todo define already connect error?
		return status.Error(codes.AlreadyExists, "already connect to another cluster, you can disconnect this connect by command `kubevpn disconnect`")
	}
	svr.t = time.Now()
	svr.connect = &handler.ConnectOptions{
		Namespace:   req.Namespace,
		Headers:     req.Headers,
		Workloads:   req.Workloads,
		ExtraCIDR:   req.ExtraCIDR,
		ExtraDomain: req.ExtraDomain,
		UseLocalDNS: req.UseLocalDNS,
		Engine:      config.Engine(req.Engine),
	}
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	var transferImage = req.TransferImage

	go util.StartupPProf(config.PProfPort)
	defaultlog.Default().SetOutput(io.Discard)
	if transferImage {
		err := util.TransferImage(ctx, sshConf, config.OriginImage, req.Image, out)
		if err != nil {
			return err
		}
	}
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})

	sshCtx, sshCancel := context.WithCancel(context.Background())
	handler.RollbackFuncList = append(handler.RollbackFuncList, sshCancel)
	err = handler.SshJumpAndSetEnv(sshCtx, sshConf, flags, false)
	if err != nil {
		return err
	}
	err = svr.connect.InitClient(InitFactory(req.KubeconfigBytes, req.Namespace))
	if err != nil {
		return err
	}
	err = svr.connect.PreCheckResource()
	if err != nil {
		return err
	}
	_, err = svr.connect.RentInnerIP(ctx)
	if err != nil {
		return err
	}

	config.Image = req.Image
	err = svr.connect.DoConnect(sshCtx)
	if err != nil {
		log.Errorf("do connect error: %v", err)
		svr.connect.Cleanup()
		return err
	}
	return nil
}

func (svr *Server) redirectToSudoDaemon(req *rpc.ConnectRequest, resp rpc.Daemon_ConnectServer) error {
	cli := svr.GetClient(true)
	if cli == nil {
		return fmt.Errorf("sudo daemon not start")
	}
	connect := &handler.ConnectOptions{
		Namespace:   req.Namespace,
		Headers:     req.Headers,
		Workloads:   req.Workloads,
		ExtraCIDR:   req.ExtraCIDR,
		ExtraDomain: req.ExtraDomain,
		UseLocalDNS: req.UseLocalDNS,
		Engine:      config.Engine(req.Engine),
	}
	var sshConf = util.ParseSshFromRPC(req.SshJump)
	file, err := util.ConvertToTempKubeconfigFile([]byte(req.KubeconfigBytes))
	if err != nil {
		return err
	}
	flags := pflag.NewFlagSet("", pflag.ContinueOnError)
	flags.AddFlag(&pflag.Flag{
		Name:     "kubeconfig",
		DefValue: file,
	})
	sshCtx, sshCancel := context.WithCancel(context.Background())
	handler.RollbackFuncList = append(handler.RollbackFuncList, sshCancel)
	err = handler.SshJumpAndSetEnv(sshCtx, sshConf, flags, true)
	if err != nil {
		return err
	}
	err = connect.InitClient(InitFactory(req.KubeconfigBytes, req.Namespace))
	if err != nil {
		return err
	}
	err = connect.PreCheckResource()
	if err != nil {
		return err
	}

	if svr.connect != nil {
		var isSameCluster bool
		isSameCluster, err = util.IsSameCluster(
			svr.connect.GetClientset().CoreV1().ConfigMaps(svr.connect.Namespace), svr.connect.Namespace,
			connect.GetClientset().CoreV1().ConfigMaps(connect.Namespace), connect.Namespace,
		)
		if err == nil && isSameCluster && svr.connect.Equal(connect) {
			// same cluster, do nothing
			log.Infof("already connect to cluster")
			return nil
		}
	}

	ctx, err := connect.RentInnerIP(resp.Context())
	if err != nil {
		return err
	}

	connResp, err := cli.Connect(ctx, req)
	if err != nil {
		return err
	}
	for {
		recv, err := connResp.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		err = resp.Send(recv)
		if err != nil {
			return err
		}
	}

	svr.t = time.Now()
	svr.connect = connect
	return nil
}

type warp struct {
	server rpc.Daemon_ConnectServer
}

func (r *warp) Write(p []byte) (n int, err error) {
	err = r.server.Send(&rpc.ConnectResponse{
		Message: string(p),
	})
	return len(p), err
}

func newWarp(server rpc.Daemon_ConnectServer) io.Writer {
	return &warp{server: server}
}

func InitFactory(kubeconfigBytes string, ns string) cmdutil.Factory {
	configFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
	configFlags.WrapConfigFn = func(c *rest.Config) *rest.Config {
		if path, ok := os.LookupEnv(config.EnvSSHJump); ok {
			bytes, err := os.ReadFile(path)
			cmdutil.CheckErr(err)
			var conf *restclient.Config
			conf, err = clientcmd.RESTConfigFromKubeConfig(bytes)
			cmdutil.CheckErr(err)
			return conf
		}
		return c
	}
	// todo optimize here
	temp, err := os.CreateTemp("", "*.json")
	if err != nil {
		return nil
	}
	err = temp.Close()
	if err != nil {
		return nil
	}
	err = os.WriteFile(temp.Name(), []byte(kubeconfigBytes), os.ModePerm)
	if err != nil {
		return nil
	}
	configFlags.KubeConfig = pointer.String(temp.Name())
	configFlags.Namespace = pointer.String(ns)
	matchVersionFlags := cmdutil.NewMatchVersionFlags(configFlags)
	return cmdutil.NewFactory(matchVersionFlags)
}
