//go:build !windows

package agent

import (
	"context"
	"errors"

	"centralbackup/internal/proto"
)

// Scheduled-task management targets the Windows Task Scheduler. On other
// platforms the feature reports itself unavailable; the GUI treats that as
// "no task panel" rather than an error.

const schedUnsupported = "scheduled-task management is only available on Windows clients"

func schedRunInventory(context.Context) proto.SchedInventory {
	return proto.SchedInventory{Available: false, Error: schedUnsupported}
}

func schedRunScript(context.Context, string) error {
	return errors.New(schedUnsupported)
}
