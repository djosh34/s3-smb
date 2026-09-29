// SPDX-License-Identifier: AGPL-3.0-only
package chunk

import (
 "bytes"
 "encoding/json"
 "fmt"
 "strings"
 "testing"
 "time"

 "github.com/djosh34/s3-smb/internal/logging"
)

func TestNativeCapacityDiagnosticsAreDecimal(t *testing.T) {
 var out bytes.Buffer
 logging.Install(&out);if err:=logging.Configure("json","debug");err!=nil {t.Fatal(err)}
 c:=Config{CacheDir:"memory",MaxUpload:1,MaxDownload:1,BlockSize:4<<20,UploadLimit:1_000_000,DownloadLimit:1_000_000,GetTimeout:time.Second}
 c.SelfCheck("logging-test")
 if c.BufferSize!=32<<20 {t.Fatalf("formatting patch changed native size: %d",c.BufferSize)}
 want:=fmt.Sprintf("%.6f MB",float64(c.BufferSize)/1e6)
 if !strings.Contains(out.String(),want)||!strings.Contains(out.String(),"8.000 Mbps") {t.Fatalf("untruthful decimal status: %s",&out)}
 if strings.Contains(out.String(),"MiB")||strings.Contains(out.String(),"32 MB") {t.Fatalf("binary units or relabelled binary number: %s",&out)}
 for _,line:=range strings.Split(strings.TrimSpace(out.String()),"\n") {var r map[string]any;if err:=json.Unmarshal([]byte(line),&r);err!=nil {t.Fatal(err)};if r["level"]!="WARN" {t.Fatalf("warning lost severity: %v",r)}}
}
