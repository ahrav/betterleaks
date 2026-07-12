//go:build linux && !filescan_uring

package sources

import "context"

func newUringProvider(
	context.Context,
	int,
	string,
	FileScanConfig,
) (uringProvider, error) {
	return nil, &backendUnsupportedError{reason: "binary was built without the filescan_uring tag"}
}
