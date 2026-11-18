const test=require('node:test');
const assert=require('node:assert/strict');
const {quotePreparedBatch}=require('./cloud_batch_quote.js');

test('cloud batch quote sends prepared references, not file bytes',async()=>{
 const docs=[{
  uploadId:'a'.repeat(32),uploadToken:'b'.repeat(64),variantId:'c'.repeat(32),
 },{
  uploadId:'d'.repeat(32),uploadToken:'e'.repeat(64),variantId:'f'.repeat(32),
 }];
 let request;
 const quote=await quotePreparedBatch('kiosk-demo',docs,async(url,opts)=>{
  request={url,opts};
  return{ok:true,json:async()=>({
   status:'READY',total_price_paise:900,
   checkout_supported:false,
   documents:[{sequence_no:1,duplex:'SIMPLEX'},
    {sequence_no:2,duplex:'DUPLEX_LONG_EDGE'}],
  })};
 });
 assert.equal(request.url,'/api/documents/batch/quote');
 assert.equal(request.opts.method,'POST');
 const payload=JSON.parse(request.opts.body);
 assert.deepEqual(payload.documents.map(d=>d.variant_id),[docs[0].variantId,docs[1].variantId]);
 assert.equal(Object.hasOwn(payload,'files'),false);
 assert.equal(quote.checkout_supported,false);
 assert.equal(quote.total_price_paise,900);
});

test('refuses duplicate and missing receipt tokens without contacting backend',async()=>{
 let count=0;
 const a={uploadId:'a'.repeat(32),uploadToken:'b'.repeat(64),variantId:'c'.repeat(32)};
 const mock=async()=>{count++;return{ok:false,status:500};};
 await assert.rejects(quotePreparedBatch('kiosk',[a,a],mock),/duplicate/);
 await assert.rejects(quotePreparedBatch('kiosk',[{...a,uploadToken:''}],mock),/receipt/);
 assert.equal(count,0);
});

test('stale confirmed quote is an explicit conflict',async()=>{
 const a={uploadId:'a'.repeat(32),uploadToken:'b'.repeat(64),variantId:'c'.repeat(32)};
 await assert.rejects(quotePreparedBatch('demo',[a],async()=>({ok:false,status:409})),
  err=>err.status===409);
});
