package store

import (
 "context"
 "errors"
 "fmt"
 "time"
)

// CloudCleanupObject is an expired, unreferenced object. The database is
// authoritative: no public upload ID or client path is accepted for deletion.
type CloudCleanupObject struct {
 Kind string
 ID string
 Key string
}

func (s *Store) ListExpiredCloudObjects(ctx context.Context, now time.Time, limit int) ([]CloudCleanupObject,error) {
 if s==nil||s.db==nil{return nil,errors.New("database unavailable")}
 if limit<1||limit>100{return nil,errors.New("invalid cleanup limit")}
 result:=make([]CloudCleanupObject,0,limit)
 // Prepared print PDFs are never deleted if referenced by ANY order,
 // including a paid, retryable or physically ambiguous print.
 q:=s.rebind(`SELECT v.variant_id,v.storage_key FROM document_variants v
  WHERE v.expires_at<=? AND v.storage_key IS NOT NULL
   AND v.storage_key<>'' AND v.state='READY'
   AND NOT EXISTS(SELECT 1 FROM order_documents d WHERE d.variant_id=v.variant_id)
  ORDER BY v.expires_at LIMIT ?`)
 rows,err:=s.db.QueryContext(ctx,q,now,limit)
 if err!=nil{return nil,err}
 for rows.Next(){
  var id,key string
  if err=rows.Scan(&id,&key);err!=nil{_ =rows.Close();return nil,err}
  result=append(result,CloudCleanupObject{Kind:"prepared",ID:id,Key:key})
 }
 if err=rows.Err();err!=nil{_ =rows.Close();return nil,err}
 _=rows.Close()
 if len(result)==limit{return result,nil}

 // Do not delete an original while a legitimate active preparation lease
 // could still be reading it, or while ANY order references its variants.
 q=s.rebind(`SELECT u.upload_id,u.object_key FROM document_uploads u
  WHERE u.expires_at<=? AND u.state IN ('UPLOADED','PENDING_UPLOAD','FAILED')
   AND NOT EXISTS(
    SELECT 1 FROM document_variants v JOIN order_documents d ON d.variant_id=v.variant_id
    WHERE v.upload_id=u.upload_id)
   AND NOT EXISTS(
    SELECT 1 FROM document_variants v WHERE v.upload_id=u.upload_id
     AND v.state='PREPARING' AND v.lease_expires_at>?)
  ORDER BY u.expires_at LIMIT ?`)
 rows,err=s.db.QueryContext(ctx,q,now,now,limit-len(result))
 if err!=nil{return nil,err}
 for rows.Next(){
  var id,key string
  if err=rows.Scan(&id,&key);err!=nil{_ =rows.Close();return nil,err}
  result=append(result,CloudCleanupObject{Kind:"original",ID:id,Key:key})
 }
 if err=rows.Err();err!=nil{_ =rows.Close();return nil,err}
 _=rows.Close()
 return result,nil
}

// Mark a record expired only after Storage has confirmed deletion or 404.
// On a Storage failure, the row remains eligible for the next cleanup tick.
func (s *Store) MarkCloudObjectExpired(ctx context.Context, item CloudCleanupObject, now time.Time) (bool,error) {
 if s==nil||s.db==nil{return false,errors.New("database unavailable")}
 var q string
 switch item.Kind {
 case "prepared":
  q=s.rebind(`UPDATE document_variants SET state='EXPIRED'
   WHERE variant_id=? AND storage_key=? AND state='READY' AND expires_at<=?
   AND NOT EXISTS(SELECT 1 FROM order_documents d
    WHERE d.variant_id=document_variants.variant_id)`)
 case "original":
  q=s.rebind(`UPDATE document_uploads SET state='EXPIRED'
   WHERE upload_id=? AND object_key=? AND expires_at<=?
   AND state IN ('UPLOADED','PENDING_UPLOAD','FAILED')
   AND NOT EXISTS(SELECT 1 FROM document_variants v JOIN order_documents d
    ON d.variant_id=v.variant_id WHERE v.upload_id=document_uploads.upload_id)
   AND NOT EXISTS(SELECT 1 FROM document_variants v WHERE v.upload_id=document_uploads.upload_id
    AND v.state='PREPARING' AND v.lease_expires_at>?)`)
 default:
  return false,fmt.Errorf("unknown cloud cleanup kind")
 }
 var resResult int64
 if item.Kind=="original"{
  res,err:=s.db.ExecContext(ctx,q,item.ID,item.Key,now,now)
  if err!=nil{return false,err}
  resResult,err=res.RowsAffected()
  return resResult==1,err
 }
 res,err:=s.db.ExecContext(ctx,q,item.ID,item.Key,now)
 if err!=nil{return false,err}
 resResult,err=res.RowsAffected()
 return resResult==1,err
}
