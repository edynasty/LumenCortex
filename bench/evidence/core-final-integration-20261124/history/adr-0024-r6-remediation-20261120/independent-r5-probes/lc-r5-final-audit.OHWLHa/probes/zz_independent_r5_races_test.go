//go:build !windows

package editing

import (
 "context"
 "errors"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "syscall"
 "testing"
)

// Overlay adds only a scheduling boundary after the production equality check, before Close.
var auditR5AfterRestoreReadback func(string, *os.File) error

func TestIndependentR5WriterProcess(t *testing.T) {
 mode := os.Getenv("LC_INDEPENDENT_R5_MODE")
 if mode == "" { return }
 path := os.Getenv("LC_INDEPENDENT_R5_PATH")
 data := []byte(os.Getenv("LC_INDEPENDENT_R5_DATA"))
 switch mode {
 case "append":
  f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0); if err != nil { t.Fatal(err) }
  if _, err = f.Write(data); err != nil { t.Fatal(err) }; if err = f.Close(); err != nil { t.Fatal(err) }
 case "rewrite":
  f, err := os.OpenFile(path, os.O_WRONLY, 0); if err != nil { t.Fatal(err) }
  if _, err = f.WriteAt(data, 0); err != nil { t.Fatal(err) }; if err = f.Truncate(int64(len(data))); err != nil { t.Fatal(err) }; if err = f.Close(); err != nil { t.Fatal(err) }
 case "truncate": if err := os.Truncate(path, 0); err != nil { t.Fatal(err) }
 case "replace":
  tmp := path+".foreign"; if err := os.WriteFile(tmp, data, 0600); err != nil { t.Fatal(err) }
  if err := os.Rename(tmp, path); err != nil { t.Fatal(err) }
 default: t.Fatalf("invalid writer mode %q", mode)
 }
 t.Logf("external_pid=%d parent_pid=%d mode=%s", os.Getpid(), os.Getppid(), mode)
}

func auditR5Foreign(t *testing.T, mode, path, data string) {
 t.Helper()
 cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestIndependentR5WriterProcess$")
 cmd.Env = append(os.Environ(), "LC_INDEPENDENT_R5_MODE="+mode, "LC_INDEPENDENT_R5_PATH="+path, "LC_INDEPENDENT_R5_DATA="+data)
 out, err := cmd.CombinedOutput(); t.Logf("writer: %s", out); if err != nil { t.Fatal(err) }
}

func TestIndependentR5RollbackRaceMatrix(t *testing.T) {
 type raceCase struct { name, when, mode, wantDisk string; wantState PublishState; foreignLost bool }
 cases := []raceCase{
  {"01_append_before_validation", "before", "append", "new-a\nFOREIGN\n", PublishExternalModification, false},
  {"02_append_after_validation_before_write", "validated", "append", "old-a\n", PublishRolledBack, true},
  {"03_append_after_write_before_readback", "written", "append", "old-a\nFOREIGN\n", PublishExternalModification, false},
  {"04_append_after_readback_before_close", "readback", "append", "old-a\nFOREIGN\n", PublishRolledBack, false},
  {"05_replacement_between_validation_and_restore", "validated", "replace", "FOREIGN\n", PublishRolledBack, false},
  {"06_same_inode_rewrite_before_validation", "before", "rewrite", "FOREIGN\n", PublishExternalModification, false},
  {"07_external_truncate_before_validation", "before", "truncate", "", PublishExternalModification, false},
  {"08_same_identity_rewrite_after_validation", "validated", "rewrite", "old-a\n", PublishRolledBack, true},
  {"10_close_failure_after_readback", "closefail", "", "old-a\n", PublishRollbackFailed, false},
  {"11_readback_descriptor_failure", "readfail", "", "old-a\n", PublishRollbackFailed, false},
 }
 for _, tc := range cases { t.Run(tc.name, func(t *testing.T) {
  root := t.TempDir(); writeFile(t, root, "a.txt", "old-a\n"); writeFile(t, root, "b.txt", "old-b\n")
  e := newTestEngine(t, root)
  e.beforeWrite = func(path string) { if filepath.Base(path)=="b.txt" { if err := os.WriteFile(path, []byte("conflict-b\n"), 0600); err != nil { t.Fatal(err) } } }
  target := filepath.Join(root,"a.txt")
  foreign := func() { auditR5Foreign(t,tc.mode,target,"FOREIGN\n") }
  e.onRollback = func(path string) error { if path==target && tc.when=="before" { foreign() }; return nil }
  e.SetPublishFaultHooks(PublishFaultHooks{
   BeforeRestoreWrite: func(path string) error { if path==target && tc.when=="validated" { foreign() }; return nil },
   AfterRestoreWrite: func(path string, f *os.File) error { if path==target { if tc.when=="written" { foreign() }; if tc.when=="readfail" { return f.Close() } }; return nil },
  })
  auditR5AfterRestoreReadback = func(path string, f *os.File) error { if path==target { if tc.when=="readback" { foreign() }; if tc.when=="closefail" { return f.Close() } }; return nil }
  defer func() { auditR5AfterRestoreReadback=nil }()
  a,b:=[]byte("new-a\n"),[]byte("new-b\n"); var receipt CommitReceipt
  ctx:=WithCommitRecorder(context.Background(),func(r CommitReceipt){receipt=r})
  result,err:=e.Commit(ctx, Plan{Actor:testActor(),Tool:"apply_patch",Changes:[]FileChange{{Path:"a.txt",Content:&a},{Path:"b.txt",Content:&b}}})
  if err==nil || CodeOf(err)!=CodeConflict { t.Fatalf("want primary CAS conflict, got %v",err) }
  actual:=readFileContent(t,root,"a.txt"); rf,ok:=findReceiptFile(receipt.Files,"a.txt"); if !ok {t.Fatal("missing a receipt")}
  t.Logf("OBSERVED disk=%q receipt=%+v result=%+v error=%v foreign_lost=%t",actual,rf,result,err,tc.foreignLost)
  if actual!=tc.wantDisk || rf.PublishState!=tc.wantState {t.Fatalf("unexpected behavior: disk=%q state=%s",actual,rf.PublishState)}
  if tc.wantState==PublishRollbackFailed || tc.wantState==PublishExternalModification { if rf.Verified || rf.NewSHA256!="" || rf.RolledBack {t.Fatalf("stale verified receipt: %+v",rf)} }
  if tc.foreignLost && strings.Contains(actual,"FOREIGN") {t.Fatal("expected PRE-02 counterexample not reached")}
 }) }
 t.Run("09_external_write_during_multifile_reverse_rollback",func(t *testing.T){
  root:=t.TempDir(); for _,n:=range []string{"a","b","c"}{writeFile(t,root,n+".txt","old-"+n+"\n")}
  e:=newTestEngine(t,root); e.beforeWrite=func(path string){if filepath.Base(path)=="c.txt" {if err:=os.WriteFile(path,[]byte("conflict-c\n"),0600);err!=nil{t.Fatal(err)}}}
  e.SetPublishFaultHooks(PublishFaultHooks{AfterRestoreWrite:func(path string,_ *os.File)error{if filepath.Base(path)=="b.txt"{auditR5Foreign(t,"append",filepath.Join(root,"a.txt"),"FOREIGN\n")};return nil}})
  a,b,c:=[]byte("new-a\n"),[]byte("new-b\n"),[]byte("new-c\n");var receipt CommitReceipt
  result,err:=e.Commit(WithCommitRecorder(context.Background(),func(r CommitReceipt){receipt=r}),Plan{Actor:testActor(),Tool:"apply_patch",Changes:[]FileChange{{Path:"a.txt",Content:&a},{Path:"b.txt",Content:&b},{Path:"c.txt",Content:&c}}})
  if err==nil{t.Fatal("want conflict")};ra,_:=findReceiptFile(receipt.Files,"a.txt");rb,_:=findReceiptFile(receipt.Files,"b.txt")
  t.Logf("OBSERVED a=%q b=%q receipt=%+v result=%+v error=%v",readFileContent(t,root,"a.txt"),readFileContent(t,root,"b.txt"),receipt,result,err)
  if readFileContent(t,root,"a.txt")!="new-a\nFOREIGN\n" || readFileContent(t,root,"b.txt")!="old-b\n" || ra.PublishState!=PublishExternalModification || rb.PublishState!=PublishRolledBack{t.Fatal("bad multi-file state")}
 })
 t.Run("12_existing_close_failure_foreign_append_preserved",func(t *testing.T){
  root:=t.TempDir();writeFile(t,root,"a.txt","old-a\n");writeFile(t,root,"b.txt","old-b\n");e:=newTestEngine(t,root)
  e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose:func(path string)error{if filepath.Base(path)=="b.txt"{auditR5Foreign(t,"append",path,"FOREIGN\n");return syscall.EIO};return nil}})
  a,b:=[]byte("new-a\n"),[]byte("new-b\n");var receipt CommitReceipt
  result,err:=e.Commit(WithCommitRecorder(context.Background(),func(r CommitReceipt){receipt=r}),Plan{Actor:testActor(),Tool:"apply_patch",Changes:[]FileChange{{Path:"a.txt",Content:&a},{Path:"b.txt",Content:&b}}})
  if !errors.Is(err,syscall.EIO){t.Fatal(err)};rf,_:=findReceiptFile(receipt.Files,"b.txt")
  t.Logf("OBSERVED a=%q b=%q receipt=%+v result=%+v error=%v",readFileContent(t,root,"a.txt"),readFileContent(t,root,"b.txt"),receipt,result,err)
  if readFileContent(t,root,"a.txt")!="old-a\n" || readFileContent(t,root,"b.txt")!="new-b\nFOREIGN\n" || rf.PublishState!=PublishExternalModification || rf.Verified{t.Fatal("R4-02-C regression remains")}
 })
}
