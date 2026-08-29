package catalog

import "context"

// LocalBackgroundBatchOptions controls cooperative admission between bounded
// Local datasource jobs. A running job is never interrupted.
type LocalBackgroundBatchOptions struct {
	BeforeJob func(context.Context) error
}
