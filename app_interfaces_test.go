package pitaya

import (
	"context"

	"github.com/alkaid/pitaya/v2/session"
	"google.golang.org/protobuf/proto"
)

type narrowInterfaceFake struct{}

func (*narrowInterfaceFake) Start()          {}
func (*narrowInterfaceFake) Shutdown()       {}
func (*narrowInterfaceFake) IsRunning() bool { return true }

func (*narrowInterfaceFake) RPC(
	context.Context,
	string,
	proto.Message,
	proto.Message,
	string,
) error {
	return nil
}

func (*narrowInterfaceFake) RPCTo(
	context.Context,
	string,
	string,
	proto.Message,
	proto.Message,
) error {
	return nil
}

func (*narrowInterfaceFake) GetSessionFromCtx(context.Context) session.SessPublic {
	return nil
}

func (*narrowInterfaceFake) GetSessionByUID(context.Context, string) (session.SessPublic, error) {
	return nil, nil
}

func (*narrowInterfaceFake) GetLocalSessionByUid(context.Context, string) session.SessPublic {
	return nil
}

func (*narrowInterfaceFake) Readiness(context.Context) ReadinessReport {
	return ReadinessReport{Ready: true}
}

var (
	_ Lifecycle         = (*narrowInterfaceFake)(nil)
	_ RPCInvoker        = (*narrowInterfaceFake)(nil)
	_ UserSessionLookup = (*narrowInterfaceFake)(nil)
	_ ReadinessProvider = (*narrowInterfaceFake)(nil)
)
