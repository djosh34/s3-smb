// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
 "bytes"
 "context"
 "errors"
 "os"
 "path/filepath"
 "sync/atomic"
 "testing"

 "github.com/djosh34/s3-smb/internal/juicefs/pkg/chunk"
 "github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
)
func TestCacheConfig(t *testing.T){
 f,err:=NewFormat("test",false,14);if err!=nil{t.Fatal(err)}
 c,err:=CacheConfig(f,t.TempDir(),nil);if err!=nil{t.Fatal(err)};if c.CacheSize!=100<<30{t.Fatal("native omitted capacity changed")}
 for _,size:=range []int64{0,1,999999,1000000,1000000000}{c,err=CacheConfig(f,"/not/usable",&size);if err!=nil{t.Fatal(err)};if c.CacheSize!=uint64(size){t.Fatal("decimal capacity truncated")};if size==0&&(c.CacheDir!="memory"||c.Prefetch!=0||c.Writeback){t.Fatal("zero retained cache not disabled")}}
 negative:=int64(-1);if _,err=CacheConfig(f,"",&negative);err==nil{t.Fatal("negative capacity accepted")}
}
type countStore struct{object.ObjectStorage;gets atomic.Int64;failPut bool}
func(s *countStore)Get(ctx context.Context,key string,off,limit int64,getters ...object.AttrGetter)(ioReadCloser interfaceReadCloser,err error){panic("unused")}
