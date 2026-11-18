package store

import (
 "errors"
 "testing"
)

func TestCloudPrintTaskStateRequiresOrderAndSequentialCompletion(t *testing.T) {
 if err:=ValidateCloudPrintTaskTransition(CloudTaskAwaitingPayment,CloudTaskReady,
  false,true,false,false);!errors.Is(err,ErrUnsafePrintTaskTransition){
  t.Fatal("later task became ready before previous completed")
 }
 if err:=ValidateCloudPrintTaskTransition(CloudTaskAwaitingPayment,CloudTaskReady,
  true,false,false,false);!errors.Is(err,ErrUnsafePrintTaskTransition){
  t.Fatal("unpaid task became ready")
 }
 for _,step:=range []struct{from,to CloudPrintTaskState;prior,paid,cups,operator bool}{
  {CloudTaskAwaitingPayment,CloudTaskReady,true,true,false,false},
  {CloudTaskReady,CloudTaskDispatching,true,true,false,false},
  {CloudTaskDispatching,CloudTaskSubmitted,true,true,true,false},
  {CloudTaskSubmitted,CloudTaskCompleted,true,true,true,false},
 }{
  if err:=ValidateCloudPrintTaskTransition(step.from,step.to,
   step.prior,step.paid,step.cups,step.operator);err!=nil{
   t.Fatalf("legitimate transition %s -> %s rejected: %v",step.from,step.to,err)
  }
 }
}

func TestCloudPrintTaskNeverBlindlyReprintsAfterUncertainCUPS(t *testing.T) {
 if err:=ValidateCloudPrintTaskTransition(CloudTaskDispatching,CloudTaskReconcile,
  true,true,false,false);err!=nil{t.Fatal(err)}
 for _,state:=range []CloudPrintTaskState{CloudTaskReady,CloudTaskDispatching,CloudTaskSubmitted}{
  if err:=ValidateCloudPrintTaskTransition(CloudTaskReconcile,state,true,true,true,false);
   !errors.Is(err,ErrUnsafePrintTaskTransition) {
   t.Fatalf("reconciliation state escaped without operator evidence to %s",state)
  }
 }
 if err:=ValidateCloudPrintTaskTransition(CloudTaskSubmitted,CloudTaskCompleted,
  true,true,false,false);!errors.Is(err,ErrUnsafePrintTaskTransition){
  t.Fatal("job completed without CUPS/physical evidence")
 }
 if err:=ValidateCloudPrintTaskTransition(CloudTaskReconcile,CloudTaskCompleted,
  true,true,true,false);!errors.Is(err,ErrUnsafePrintTaskTransition){
  t.Fatal("ambiguous output auto-completed without operator reconciliation")
 }
 if err:=ValidateCloudPrintTaskTransition(CloudTaskReconcile,CloudTaskCompleted,
  true,true,true,true);err!=nil{t.Fatal(err)}
 for _,state:=range []CloudPrintTaskState{CloudTaskReady,CloudTaskDispatching,CloudTaskSubmitted}{
  if err:=ValidateCloudPrintTaskTransition(CloudTaskCompleted,state,true,true,true,true);
   !errors.Is(err,ErrUnsafePrintTaskTransition){
   t.Fatalf("completed paper job re-dispatched to %s",state)
  }
 }
}
