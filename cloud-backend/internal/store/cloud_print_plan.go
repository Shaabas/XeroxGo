package store

import (
 "errors"
 "fmt"
 "math"
 "strings"
)

// CloudPrintTask is the immutable, per-document physical print specification.
// Canonical preparation already bakes requested copies into the PDF, so the
// printer is ALWAYS instructed to make exactly one copy of these bytes.
type CloudPrintTask struct {
 SequenceNo int
 VariantID string
 Checksum string
 StorageKey string
 Duplex string
 ColorMode string
 Copies int
 TotalPages int
 TotalSheets int
 PricePaise int64
 TariffRevision int64
}

type CloudPrintPlan struct {
 Tasks []CloudPrintTask
 TotalPricePaise int64
 TotalPages int
 TotalSheets int
 TariffRevision int64
}

// BuildCloudPrintPlan constructs an ordered sequence of separate CUPS tasks.
// It NEVER merges PDFs or collapses duplex/color settings to a global value.
// Callers must first load READY variants from the authoritative database;
// the planned tasks are only an in-memory validation artifact until the
// future multi-file checkout transaction binds them to the same paid order.
func BuildCloudPrintPlan(variants []CloudDocumentVariant) (CloudPrintPlan,error) {
 var plan CloudPrintPlan
 if len(variants)<1||len(variants)>20 {
  return plan,fmt.Errorf("cloud order requires 1-20 prepared documents")
 }
 seen:=make(map[string]bool,len(variants))
 for index,v:=range variants {
  if v.State!="READY"||v.ID==""||seen[v.ID] {
   return CloudPrintPlan{},errors.New("duplicate or unavailable prepared variant")
  }
  seen[v.ID]=true
  if len(v.Checksum)!=64||!validCloudPlanSHA(v.Checksum)||
   v.StorageKey!="variants/"+v.ID+"/output.pdf"||v.Copies<1||
   v.BilledSides<1||v.TotalSheets<1||v.PricePaise<1||v.TariffRevision<1 {
   return CloudPrintPlan{},errors.New("incomplete canonical print variant")
  }
  if v.Duplex!="SIMPLEX"&&v.Duplex!="DUPLEX_LONG_EDGE"&&v.Duplex!="DUPLEX_SHORT_EDGE" {
   return CloudPrintPlan{},errors.New("unsupported prepared duplex mode")
  }
  if v.ColorMode!="BW"&&v.ColorMode!="COLOR" {
   return CloudPrintPlan{},errors.New("unsupported prepared color mode")
  }
  if index==0 {
   plan.TariffRevision=v.TariffRevision
  }else if plan.TariffRevision!=v.TariffRevision{
   return CloudPrintPlan{},errors.New("mixed tariff revision requires re-quoting")
  }
  if plan.TotalPricePaise>math.MaxInt64-v.PricePaise ||
   plan.TotalPages>math.MaxInt-v.BilledSides||
   plan.TotalSheets>math.MaxInt-v.TotalSheets {
   return CloudPrintPlan{},errors.New("cloud print plan totals overflow")
  }
  plan.TotalPricePaise+=v.PricePaise
  plan.TotalPages+=v.BilledSides
  plan.TotalSheets+=v.TotalSheets
  plan.Tasks=append(plan.Tasks,CloudPrintTask{
   SequenceNo:index+1,VariantID:v.ID,Checksum:strings.ToLower(v.Checksum),
   StorageKey:v.StorageKey,Duplex:v.Duplex,ColorMode:v.ColorMode,Copies:1,
   TotalPages:v.BilledSides,TotalSheets:v.TotalSheets,
   PricePaise:v.PricePaise,TariffRevision:v.TariffRevision,
  })
 }
 return plan,nil
}

func validCloudPlanSHA(s string)bool{
 for _,c:=range s{
  if (c<'0'||c>'9')&&(c<'a'||c>'f')&&(c<'A'||c>'F'){return false}
 }
 return true
}
