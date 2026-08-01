//go:build !unix

package catalog

import "os"

func localFileIdentity(info os.FileInfo) string { return "" }
