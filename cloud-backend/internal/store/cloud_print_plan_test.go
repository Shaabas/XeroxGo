package store

import (
 "math"
 "strings"
 "testing"
)

func sampleCloudPlanVariant(id,duplex,color string,price int64) CloudDocumentVariant {
 return CloudDocumentVariant{
  ID:id,StorageKey:"variants/"+id+"/output.pdf",
  Checksum:strings.Repeat("a",64),State:"READY",
  Copies:2,Duplex:duplex,ColorMode:color,BilledSides:3,
  TotalSheets:2,PricePaise:price,TariffRevision:5,
 }
}

func TestCloudPrintPlanPreservesMixedDuplexAndColor(t *testing.T){
 inputs:=[]CloudDocumentVariant{
  sampleCloudPlanVariant("a","SIMPLEX","BW",500),
  sampleCloudPlanVariant("b","DUPLEX_LONG_EDGE","COLOR",600),
  sampleCloudPlanVariant("c","DUPLEX_SHORT_EDGE","BW",700),
 }
 plan,err:=BuildCloudPrintPlan(inputs)
 if err!=nil{t.Fatal(err)}
 if len(plan.Tasks)!=3||plan.TotalPricePaise!=1800||
  plan.TotalSheets!=6||plan.TotalPages!=9||plan.TariffRevision!=5{
  t.Fatalf("unexpected totals %+v",plan)
 }
 for i,task:=range plan.Tasks{
  if task.SequenceNo!=i+1||task.VariantID!=inputs[i].ID||
   task.Duplex!=inputs[i].Duplex||task.ColorMode!=inputs[i].ColorMode||
   task.Copies!=1||task.Checksum!=inputs[i].Checksum{
   t.Fatalf("task %d lost physical settings: %+v",i,task)
  }
 }
}

func TestCloudPrintPlanRejectsUnverifiedOrInconsistentVariants(t *testing.T){
 a:=sampleCloudPlanVariant("a","SIMPLEX","BW",100)
 b:=sampleCloudPlanVariant("b","DUPLEX_LONG_EDGE","COLOR",100)
 bad:=[]struct{
  name string
  mutate func(*CloudDocumentVariant)
 }{
  {"unready",func(v *CloudDocumentVariant){v.State="PREPARING"}},
  {"bad digest",func(v *CloudDocumentVariant){v.Checksum=strings.Repeat("x",64)}},
  {"spoofed path",func(v *CloudDocumentVariant){v.StorageKey="uploads/other/original.pdf"}},
  {"negative price",func(v *CloudDocumentVariant){v.PricePaise=-1}},
  {"invalid duplex",func(v *CloudDocumentVariant){v.Duplex="INVALID"}},
  {"invalid color",func(v *CloudDocumentVariant){v.ColorMode="RGB"}},
  {"zero sheet count",func(v *CloudDocumentVariant){v.TotalSheets=0}},
  {"missing tariff",func(v *CloudDocumentVariant){v.TariffRevision=0}},
 }
 for _,tc:=range bad {
  t.Run(tc.name,func(t *testing.T){
   candidate:=b
   tc.mutate(&candidate)
   if _,err:=BuildCloudPrintPlan([]CloudDocumentVariant{a,candidate});err==nil{
    t.Fatalf("accepted unsafe variant %+v",candidate)
   }
  })
 }
 if _,err:=BuildCloudPrintPlan([]CloudDocumentVariant{a,a});err==nil{
  t.Fatal("same variant ordered twice")
 }
 b.TariffRevision=6
 if _,err:=BuildCloudPrintPlan([]CloudDocumentVariant{a,b});err==nil{
  t.Fatal("mixed tariff revisions accepted")
 }
}

func TestCloudPrintPlanRejectsOverflowAndExcessDocumentCount(t *testing.T){
 huge:=sampleCloudPlanVariant("huge","SIMPLEX","BW",math.MaxInt64-1)
 small:=sampleCloudPlanVariant("small","SIMPLEX","BW",3)
 if _,err:=BuildCloudPrintPlan([]CloudDocumentVariant{huge,small});err==nil{
  t.Fatal("overflowed order price")
 }
 many:=make([]CloudDocumentVariant,21)
 for i:=range many {
  many[i]=sampleCloudPlanVariant(string(rune('a'+i)),"SIMPLEX","BW",10)
 }
 if _,err:=BuildCloudPrintPlan(many);err==nil{
  t.Fatal("more than 20 documents accepted")
 }
 if _,err:=BuildCloudPrintPlan(nil);err==nil{
  t.Fatal("empty print plan accepted")
 }
}
