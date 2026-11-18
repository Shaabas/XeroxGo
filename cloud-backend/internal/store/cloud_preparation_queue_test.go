package store

import (
 "context"
 "path/filepath"
 "testing"
 "time"
)

func cloudQueueFixture(t *testing.T)*Store{
 t.Helper()
 db,err:=Open(filepath.Join(t.TempDir(),"cloud_queue.db"))
 if err!=nil{t.Fatal(err)}
 t.Cleanup(func(){_ =db.Close()})
 q:=`CREATE TABLE cloud_preparation_jobs (
   job_id TEXT PRIMARY KEY,upload_id TEXT,settings_hash TEXT,
   normalizer_version TEXT,options_json TEXT,tariff_revision BIGINT,
   variant_id TEXT,state TEXT,attempts INTEGER,lease_expires_at TIMESTAMP,
   created_at TIMESTAMP,updated_at TIMESTAMP,
   UNIQUE(upload_id,settings_hash,normalizer_version))`
 if _,err=db.DB().Exec(q);err!=nil{t.Fatal(err)}
 return db
}

func TestCloudPreparationEnqueueAndSingleClaim(t *testing.T){
 db:=cloudQueueFixture(t)
 ctx:=context.Background()
 candidate:=CloudPreparationJob{ID:"job-a",UploadID:"upload-a",
  SettingsHash:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  NormalizerVersion:"canonical-v1-tariff-1",OptionsJSON:`{"Copies":1}`,
  TariffRevision:1}
 first,err:=db.EnqueueCloudPreparation(ctx,candidate)
 if err!=nil||first.ID!="job-a"||first.State!="QUEUED"{t.Fatalf("enqueue %+v %v",first,err)}
 candidate.ID="different-id"
 second,err:=db.EnqueueCloudPreparation(ctx,candidate)
 if err!=nil||second.ID!="job-a"{t.Fatalf("dedupe %+v %v",second,err)}
 claimed,err:=db.ClaimNextCloudPreparation(ctx,time.Now().UTC())
 if err!=nil||claimed==nil||claimed.ID!="job-a"||claimed.Attempts!=1||
  claimed.State!="PROCESSING" {t.Fatalf("claim %+v %v",claimed,err)}
 competing,err:=db.ClaimNextCloudPreparation(ctx,time.Now().UTC())
 if err!=nil||competing!=nil{t.Fatalf("double claim %+v %v",competing,err)}
 ok,err:=db.FinishCloudPreparation(ctx,"job-a",1,"variant-a","READY")
 if err!=nil||!ok{t.Fatalf("finish %v %v",ok,err)}
 status,err:=db.GetCloudPreparationByID(ctx,"upload-a","job-a")
 if err!=nil||status.State!="READY"||status.VariantID!="variant-a" {
  t.Fatalf("final %+v %v",status,err)
 }
 if _,err=db.GetCloudPreparationByID(ctx,"another-upload","job-a");err==nil{
  t.Fatal("cross-upload queue access accepted")
 }
}

func TestCloudPreparationRestartReclaimsExpiredLease(t *testing.T){
 db:=cloudQueueFixture(t)
 ctx:=context.Background()
 job:=CloudPreparationJob{ID:"restart-a",UploadID:"upload-a",
  SettingsHash:"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  NormalizerVersion:"canonical-v1-tariff-1",OptionsJSON:`{"Copies":1}`,
  TariffRevision:1}
 if _,err:=db.EnqueueCloudPreparation(ctx,job);err!=nil{t.Fatal(err)}
 now:=time.Now().UTC()
 first,err:=db.ClaimNextCloudPreparation(ctx,now)
 if err!=nil||first==nil||first.Attempts!=1{t.Fatalf("first %+v %v",first,err)}
 if next,err:=db.ClaimNextCloudPreparation(ctx,now.Add(time.Minute));err!=nil||next!=nil{
  t.Fatalf("live worker stolen %+v %v",next,err)
 }
 retry,err:=db.ClaimNextCloudPreparation(ctx,now.Add(11*time.Minute))
 if err!=nil||retry==nil||retry.Attempts!=2{t.Fatalf("restart claim %+v %v",retry,err)}
 stale,err:=db.FinishCloudPreparation(ctx,first.ID,first.Attempts,"old-variant","READY")
 if err!=nil||stale{t.Fatalf("old worker published after lease recovery %v %v",stale,err)}
 if err:=db.RetryCloudPreparation(ctx,retry.ID,retry.Attempts);err!=nil{t.Fatal(err)}
 third,err:=db.ClaimNextCloudPreparation(ctx,time.Now().UTC())
 if err!=nil||third==nil||third.Attempts!=3{t.Fatalf("third attempt %+v %v",third,err)}
 if err:=db.RetryCloudPreparation(ctx,third.ID,third.Attempts);err!=nil{t.Fatal(err)}
 terminal,err:=db.GetCloudPreparationByID(ctx,job.UploadID,job.ID)
 if err!=nil||terminal.State!="FAILED"{t.Fatalf("failed terminal state %+v %v",terminal,err)}
 if extra,err:=db.ClaimNextCloudPreparation(ctx,now.Add(30*time.Minute));err!=nil||extra!=nil{
  t.Fatalf("too many attempts %+v %v",extra,err)
 }
}


func TestCloudQueueExhaustedCrashReconcilesToFailed(t *testing.T) {
 db:=cloudQueueFixture(t)
 ctx:=context.Background()
 job:=CloudPreparationJob{ID:"crashed",UploadID:"upload-1",
  SettingsHash:"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
  NormalizerVersion:"canonical-v1-tariff-1",OptionsJSON:`{"Copies":1}`,
  TariffRevision:1}
 if _,err:=db.EnqueueCloudPreparation(ctx,job);err!=nil{t.Fatal(err)}
 past:=time.Now().UTC().Add(-time.Minute)
 _,err:=db.DB().Exec(`UPDATE cloud_preparation_jobs
  SET state='PROCESSING',attempts=3,lease_expires_at=?
  WHERE job_id='crashed'`,past)
 if err!=nil{t.Fatal(err)}
 n,err:=db.FailExhaustedCloudPreparations(ctx,time.Now().UTC())
 if err!=nil||n!=1{t.Fatalf("exhausted not reaped %d %v",n,err)}
 result,err:=db.GetCloudPreparationByID(ctx,job.UploadID,job.ID)
 if err!=nil||result.State!="FAILED"{
  t.Fatalf("unexpected reconciled state %+v %v",result,err)
 }
}
