package server

import (
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// AAPL treats an empty named stream as absent for FILE_OPEN only. Other
// dispositions still use Lookup's selected-object existence without alteration.
func streamOpenStatus(request RequestContext, create wire.CreateRequest, resolved smb.Resolved) smb.Status {
	if request.aaplNegotiated() && create.Disposition == fileOpen && resolved.Exists && resolved.Name.Stream != "" && resolved.Attr.Size == 0 {
		return smb.StatusObjectNameNotFound
	}
	return smb.StatusSuccess
}
