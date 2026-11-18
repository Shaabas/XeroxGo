package store

import (
 "context"
 "path/filepath"
 "testing"
 "time"
)

func TestCloudCleanupKeepsReferencedAndActivelyPreparedDocuments(t *testing.T){
 db,err:=Open(filepath.Join(t.TempDir(),"cleanup.sqlite"))
 if err!=nil{t.Fatal(err)}
 defer db.Close()
 for _,q:=range []string{
  `CREATE TABLE document_uploads (
   upload_id TEXT PRIMARY KEY,object_key TEXT,state TEXT,
   expires_at TIMESTAMP)`,
  `CREATE TABLE document_variants (
   variant_id TEXT PRIMARY KEY,upload_id TEXT,storage_key TEXT,state TEXT,
   expires_at TIMESTAMP,lease_expires_at TIMESTAMP)`,
  `CREATE TABLE order_documents (order_id TEXT,sequence_no INTEGER,variant_id TEXT)`,
 } {
  if _,err:=db.DB().Exec(q);err!=nil{t.Fatal(err)}
 }
 now:=time.Now().UTC()
 past:=now.Add(-time.Hour)
 live:=now.Add(time.Hour)
 for _,id:=range []string{"paid","orphan","processing"}{
  if _,err:=db.DB().Exec(`INSERT INTO document_uploads
  (upload_id,object_key,state,expires_at) VALUES(?,?,?,?)`,
   id,"uploads/"+id+"/original.pdf","UPLOADED",past);err!=nil{t.Fatal(err)}
 }
 for _,v:=range []struct{id,upload,state string;expires,lease time.Time}{
  {"v-paid","paid","READY",past,past},
  {"v-orphan","orphan","READY",past,past},
  {"v-working","processing","PREPARING",past,live},
 }{
  if _,err:=db.DB().Exec(`INSERT INTO document_variants
   (variant_id,upload_id,storage_key,state,expires_at,lease_expires_at)
   VALUES(?,?,?,?,?,?)`,v.id,v.upload,"variants/"+v.id+"/output.pdf",
   v.state,v.expires,v.lease);err!=nil{t.Fatal(err)}
 }
 if _,err:=db.DB().Exec(`INSERT INTO order_documents
 (order_id,sequence_no,variant_id) VALUES('order-paid',1,'v-paid')`);err!=nil{t.Fatal(err)}
 objs,err:=db.ListExpiredCloudObjects(context.Background(),now,25)
 if err!=nil{t.Fatal(err)}
 found:=map[string]bool{}
 for _,o:=range objs{found[o.Kind+":"+o.ID]=true}
 if found["prepared:v-paid"]||found["original:paid"]||
  found["prepared:v-working"]||found["original:processing"] {
  t.Fatalf("unsafe cleanup candidates: %+v",objs)
 }
 if !found["prepared:v-orphan"]||!found["original:orphan"] {
  t.Fatalf("missed unreferenced garbage: %+v",objs)
 }
 ok,err:=db.MarkCloudObjectExpired(context.Background(),CloudCleanupObject{
  Kind:"prepared",ID:"v-paid",Key:"variants/v-paid/output.pdf"},now)
 if err!=nil||ok{t.Fatalf("marked ordered PDF expired: %v %v",ok,err)}
 for _,obj:=range objs{
  ok,err=db.MarkCloudObjectExpired(context.Background(),obj,now)
  if err!=nil||!ok{t.Fatalf("failed cleanup marking %+v: %v %v",obj,ok,err)}
 }
 objs,err=db.ListExpiredCloudObjects(context.Background(),now,25)
 if err!=nil||len(objs)!=0{t.Fatalf("cleanup repeatedly selected finished work %+v %v",objs,err)}
}
