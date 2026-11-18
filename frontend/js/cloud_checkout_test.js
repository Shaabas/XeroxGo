const test=require('node:test');
const assert=require('node:assert/strict');
const {newCheckoutRequestID,placePreparedOrder}=require('./cloud_checkout.js');
const input={
 kioskId:'kiosk-1',uploadId:'a'.repeat(32),uploadToken:'b'.repeat(64),
 variantId:'c'.repeat(32),requestId:'d'.repeat(32),
 pricePaise:500,tariffRevision:1,
};
test('reference-only checkout transmits identifiers, not file bytes',async()=>{
 const requests=[];
 const reply=await placePreparedOrder(input,async(url,options)=>{
  requests.push({url,options});
  return {ok:true,json:async()=>({order_id:'ord-1',display_code:'0123'})};
 });
 assert.equal(reply.display_code,'0123');
 assert.equal(requests.length,1);
 assert.equal(requests[0].url,'/api/documents/checkout');
 assert.equal(requests[0].options.method,'POST');
 const payload=JSON.parse(requests[0].options.body);
 assert.equal(payload.request_id,input.requestId);
 assert.equal(payload.variant_id,input.variantId);
 assert.equal(payload.accepted_total_price_paise,500);
 assert.equal(Object.prototype.hasOwnProperty.call(payload,'files'),false);
});
test('request identifier is stable when retrying a failed checkout',async()=>{
 const seen=[];
 const fail=async(_url,options)=>{seen.push(JSON.parse(options.body).request_id);return {ok:false,status:503};};
 for(let i=0;i<2;i++){
  await assert.rejects(placePreparedOrder(input,fail),err=>{
   assert.equal(err.requestId,input.requestId);
   return true;
  });
 }
 assert.deepEqual(seen,[input.requestId,input.requestId]);
});
test('client refuses missing credentials and invented IDs',async()=>{
 let called=0;
 await assert.rejects(placePreparedOrder({...input,requestId:''},async()=>{called++;}),/incomplete/);
 assert.equal(called,0);
 assert.equal(newCheckoutRequestID(()=> '12345678-1234-1234-1234-123456789abc'),
  '12345678123412341234123456789abc');
});
