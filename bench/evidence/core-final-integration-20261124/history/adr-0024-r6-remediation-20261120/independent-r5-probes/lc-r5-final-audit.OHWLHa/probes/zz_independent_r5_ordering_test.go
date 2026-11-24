//go:build !windows
package editing
import("context";"os";"os/exec";"path/filepath";"strings";"testing")
var auditR5AfterRestorePayloadWrite func(string) error
func TestIndependentR5AppendAfterRestoreWriteHelper(t *testing.T){
 p:=os.Getenv("LC_R5_ORDERING_TARGET");if p==""{return};f,e:=os.OpenFile(p,os.O_WRONLY|os.O_APPEND,0);if e!=nil{t.Fatal(e)};if _,e=f.WriteString("FOREIGN\n");e!=nil{t.Fatal(e)};if e=f.Close();e!=nil{t.Fatal(e)};t.Logf("external_pid=%d parent=%d",os.Getpid(),os.Getppid())
}
// Deliberately observes both versions; PASS only means the comparison was executed.
func TestIndependentR5AppendAfterRestoreWrite(t *testing.T){
 root:=t.TempDir();writeFile(t,root,"a.txt","old-a\n");writeFile(t,root,"b.txt","old-b\n");e:=newTestEngine(t,root)
 e.beforeWrite=func(path string){if filepath.Base(path)=="b.txt"{if err:=os.WriteFile(path,[]byte("conflict-b\n"),0600);err!=nil{t.Fatal(err)}}}
 hit:=false;auditR5AfterRestorePayloadWrite=func(path string)error{if filepath.Base(path)!="a.txt"{return nil};hit=true;cmd:=exec.Command(os.Args[0],"-test.v","-test.run=^TestIndependentR5AppendAfterRestoreWriteHelper$");cmd.Env=append(os.Environ(),"LC_R5_ORDERING_TARGET="+path);out,err:=cmd.CombinedOutput();t.Logf("writer %s",out);return err};defer func(){auditR5AfterRestorePayloadWrite=nil}()
 a,b:=[]byte("new-a\n"),[]byte("new-b\n");result,err:=e.Commit(context.Background(),Plan{Actor:testActor(),Tool:"apply_patch",Changes:[]FileChange{{Path:"a.txt",Content:&a},{Path:"b.txt",Content:&b}}})
 if err==nil || !hit{t.Fatalf("not reached error=%v hit=%t",err,hit)}
 disk:=readFileContent(t,root,"a.txt");lost:=!strings.Contains(disk,"FOREIGN\n");t.Logf("OBSERVED disk=%q foreign_lost=%t result=%+v err=%v",disk,lost,result,err)
 if expected:=os.Getenv("LC_R5_EXPECT_FOREIGN_LOST");expected!="" && (expected=="true")!=lost{t.Fatalf("unexpected outcome wantLost=%s disk=%q",expected,disk)}
}
