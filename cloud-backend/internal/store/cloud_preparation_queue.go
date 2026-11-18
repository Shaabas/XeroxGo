package store

import (
 "context"
 "database/sql"
 "errors"
 "fmt"
 "time"
)

type CloudPreparationJob struct {
 ID string
 UploadID string
 SettingsHash string
 NormalizerVersion string
 OptionsJSON string
 TariffRevision int64
 VariantID string
 State string
 Attempts int
 LeaseExpiresAt time.Time
}

func (s *Store) EnqueueCloudPreparation(ctx context.Context, job CloudPreparationJob) (CloudPreparationJob,error) {
 if s==nil||s.db==nil{return job,errors.New("database unavailable")}
 if job.ID==""||job.UploadID==""||len(job.SettingsHash)!=64||job.NormalizerVersion==""||
  job.TariffRevision<=0||len(job.OptionsJSON)<2||len(job.OptionsJSON)>4096{
  return job,errors.New("invalid preparation job")
 }
 now:=time.Now().UTC()
 query:=s.rebind(`INSERT INTO cloud_preparation_jobs
  (job_id,upload_id,settings_hash,normalizer_version,options_json,tariff_revision,
   state,attempts,created_at,updated_at)
  VALUES(?,?,?,?,?,?,'QUEUED',0,?,?)
  ON CONFLICT(upload_id,settings_hash,normalizer_version) DO NOTHING`)
 _,err:=s.db.ExecContext(ctx,query,job.ID,job.UploadID,job.SettingsHash,
  job.NormalizerVersion,job.OptionsJSON,job.TariffRevision,now,now)
 if err!=nil{return job,err}
 return s.GetCloudPreparation(ctx,job.UploadID,job.SettingsHash,job.NormalizerVersion)
}

func scanCloudPrep(row *sql.Row) (CloudPreparationJob,error) {
 var j CloudPreparationJob
 var variant sql.NullString
 var lease sql.NullTime
 err:=row.Scan(&j.ID,&j.UploadID,&j.SettingsHash,&j.NormalizerVersion,
  &j.OptionsJSON,&j.TariffRevision,&variant,&j.State,&j.Attempts,&lease)
 if variant.Valid{j.VariantID=variant.String}
 if lease.Valid{j.LeaseExpiresAt=lease.Time}
 return j,err
}

func (s *Store) GetCloudPreparation(ctx context.Context,upload,settings,version string) (CloudPreparationJob,error) {
 var j CloudPreparationJob
 if s==nil||s.db==nil{return j,errors.New("database unavailable")}
 q:=s.rebind(`SELECT job_id,upload_id,settings_hash,normalizer_version,options_json,
  tariff_revision,variant_id,state,attempts,lease_expires_at FROM cloud_preparation_jobs
  WHERE upload_id=? AND settings_hash=? AND normalizer_version=?`)
 j,err:=scanCloudPrep(s.db.QueryRowContext(ctx,q,upload,settings,version))
 if errors.Is(err,sql.ErrNoRows){return j,ErrVariantUnavailable}
 return j,err
}

func (s *Store) GetCloudPreparationByID(ctx context.Context,upload,id string) (CloudPreparationJob,error) {
 var j CloudPreparationJob
 if s==nil||s.db==nil{return j,errors.New("database unavailable")}
 q:=s.rebind(`SELECT job_id,upload_id,settings_hash,normalizer_version,options_json,
  tariff_revision,variant_id,state,attempts,lease_expires_at FROM cloud_preparation_jobs
  WHERE upload_id=? AND job_id=?`)
 j,err:=scanCloudPrep(s.db.QueryRowContext(ctx,q,upload,id))
 if errors.Is(err,sql.ErrNoRows){return j,ErrVariantUnavailable}
 return j,err
}

// One atomic UPDATE ... RETURNING is the distributed worker claim. PostgreSQL
// and SQLite serialize competing updates; a lost race returns no job.
func (s *Store) ClaimNextCloudPreparation(ctx context.Context,now time.Time) (*CloudPreparationJob,error) {
 if s==nil||s.db==nil{return nil,errors.New("database unavailable")}
 // The job lease must outlast the variant lease (10 minutes) to prevent
 // a restarted worker from observing a still-live stale normalization.
 deadline:=now.Add(11*time.Minute)
 q:=s.rebind(`UPDATE cloud_preparation_jobs
  SET state='PROCESSING',attempts=attempts+1,lease_expires_at=?,updated_at=?
  WHERE job_id=(
   SELECT job_id FROM cloud_preparation_jobs
   WHERE attempts<3 AND (
    state='QUEUED' OR (state='PROCESSING' AND lease_expires_at<=?))
   ORDER BY created_at,job_id LIMIT 1)
  AND attempts<3
  AND (state='QUEUED' OR (state='PROCESSING' AND lease_expires_at<=?))
  RETURNING job_id,upload_id,settings_hash,normalizer_version,options_json,
    tariff_revision,variant_id,state,attempts,lease_expires_at`)
 row:=s.db.QueryRowContext(ctx,q,deadline,now,now,now)
 var j CloudPreparationJob
 var v sql.NullString
 var lease sql.NullTime
 err:=row.Scan(&j.ID,&j.UploadID,&j.SettingsHash,&j.NormalizerVersion,
  &j.OptionsJSON,&j.TariffRevision,&v,&j.State,&j.Attempts,&lease)
 if errors.Is(err,sql.ErrNoRows){return nil,nil}
 if err!=nil{return nil,fmt.Errorf("claim cloud preparation: %w",err)}
 if v.Valid{j.VariantID=v.String}
 if lease.Valid{j.LeaseExpiresAt=lease.Time}
 return &j,nil
}

func (s *Store) FinishCloudPreparation(ctx context.Context,id string,attempt int,variantID,state string) (bool,error) {
 if s==nil||s.db==nil{return false,errors.New("database unavailable")}
 if id==""||attempt<1||(state!="READY"&&state!="FAILED") {
  return false,errors.New("invalid completed preparation")
 }
 now:=time.Now().UTC()
 q:=s.rebind(`UPDATE cloud_preparation_jobs SET state=?,variant_id=?,
  lease_expires_at=NULL,updated_at=?
  WHERE job_id=? AND state='PROCESSING' AND attempts=? AND lease_expires_at>?`)
 res,err:=s.db.ExecContext(ctx,q,state,variantID,now,id,attempt,now)
 if err!=nil{return false,err}
 n,err:=res.RowsAffected()
 return n==1,err
}

// On a failed attempt, let the next worker retry without waiting 10 minutes.
// No queue row is ever deleted while requests may be recovering.
func (s *Store) RetryCloudPreparation(ctx context.Context,id string,attempt int) error {
 if s==nil||s.db==nil{return errors.New("database unavailable")}
 state:="QUEUED"
 if attempt>=3{state="FAILED"}
 q:=s.rebind(`UPDATE cloud_preparation_jobs SET state=?,lease_expires_at=NULL,
  updated_at=? WHERE job_id=? AND attempts=? AND state='PROCESSING'`)
 _,err:=s.db.ExecContext(ctx,q,state,time.Now().UTC(),id,attempt)
 return err
}


// Mark a worker whose final allowed attempt crashed as FAILED rather than
// leaving an eternal PROCESSING/RECOVERING status with no eligible claimant.
func (s *Store) FailExhaustedCloudPreparations(ctx context.Context,now time.Time) (int64,error){
 if s==nil||s.db==nil{return 0,errors.New("database unavailable")}
 q:=s.rebind(`UPDATE cloud_preparation_jobs
   SET state='FAILED',lease_expires_at=NULL,updated_at=?
   WHERE state='PROCESSING' AND attempts>=3 AND lease_expires_at<=?`)
 res,err:=s.db.ExecContext(ctx,q,now,now)
 if err!=nil{return 0,err}
 return res.RowsAffected()
}
