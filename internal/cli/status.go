package cli

import (
	"context"
	"io"

	"github.com/snonux/f3sctl/internal/presenter"
)

// printStatus renders the probe results as an aligned table.
//
// Takes powerEngine rather than *power.Engine so it has the same signature as
// every other powerAction (see cli.go): the read-only verb needs no adapter,
// but it still has to fit the same seam the on/off actions do.
//
// The table itself is built by internal/presenter, shared with the --remote
// client (internal/client.showStatus) so the two surfaces cannot drift the
// way they had before this was unified -- see ry0. This local path renders
// with ShowRole: true because power.Engine.ProbeAll always populates
// HostStatus.Role from the inventory; the remote client cannot, and says why
// in presenter.Options.ShowRole's doc comment.
func printStatus(ctx context.Context, eng powerEngine, out io.Writer) error {
	statuses := eng.ProbeAll(ctx)

	// The fan state is part of "is the rack healthy", so it belongs in the
	// same glance. A failure to read it is reported but is not fatal: the
	// host table above is still useful and is the more important half --
	// presenter.Status renders it as "unknown" rather than failing outright.
	fans, fansErr := eng.FansStatus(ctx)
	return presenter.Status(out, statuses, presenter.Options{ShowRole: true}, fans, fansErr)
}
