package main

import (
 "os"
 "path/filepath"
 "testing"
)
func TestCoverageFailsClosed(t *testing.T) {
 for _, tc := range []struct{name,events,ledger string; wantError bool}{
  {"missing", "", "recovery\tapp/TestRecovery\n", true},
  {"skipped", "{\"Action\":\"skip\",\"Package\":\"app\",\"Test\":\"TestRecovery\"}\n", "recovery\tapp/TestRecovery\n", true},
  {"wrong-package", "{\"Action\":\"pass\",\"Package\":\"fake\",\"Test\":\"TestRecovery\"}\n", "recovery\tapp/TestRecovery\n", true},
  {"passing", "{\"Action\":\"pass\",\"Package\":\"app\",\"Test\":\"TestRecovery\"}\n", "recovery\tapp/TestRecovery\n", false},
  {"empty-ledger", "", "# none\n", true},
 } { t.Run(tc.name,func(t *testing.T){ d:=t.TempDir(); events:=filepath.Join(d,"events"); ledger:=filepath.Join(d,"ledger"); if err:=os.WriteFile(events,[]byte(tc.events),0600);err!=nil{t.Fatal(err)}; if err:=os.WriteFile(ledger,[]byte(tc.ledger),0600);err!=nil{t.Fatal(err)}; if err:=check(events,ledger); (err!=nil)!=tc.wantError{t.Fatalf("error=%v wantError=%v",err,tc.wantError)} }) }
}
