/**
 * XG-CLOUD-DOCS-V2: read-only cloud batch quotation transport.
 * Not wired into the live cart until the print fidelity gate passes.
 * Never sends PDF bytes; browser totals are display-only.
 */
(function(root){
 'use strict';
 async function quotePreparedBatch(kioskId,documents,request=fetch){
  if(typeof kioskId!=='string'||!kioskId||!Array.isArray(documents)||
   documents.length<1||documents.length>20){
   throw new Error('Invalid prepared document batch');
  }
  const seen=new Set();
  const receipt=[];
  for(const doc of documents){
   if(!doc||typeof doc.uploadId!=='string'||doc.uploadId.length!==32||
    typeof doc.uploadToken!=='string'||doc.uploadToken.length!==64||
    typeof doc.variantId!=='string'||doc.variantId.length!==32||
    seen.has(doc.variantId)){
    throw new Error('Missing or duplicate prepared document receipt');
   }
   seen.add(doc.variantId);
   receipt.push({
    upload_id:doc.uploadId,upload_token:doc.uploadToken,
    variant_id:doc.variantId,
   });
  }
  const response=await request('/api/documents/batch/quote',{
   method:'POST',headers:{'Content-Type':'application/json'},
   body:JSON.stringify({kiosk_id:kioskId,documents:receipt}),
  });
  if(!response.ok){
   const error=new Error(response.status===409
    ? 'Prepared documents or prices have changed; refresh this quote.'
    : 'Cloud batch quote is unavailable.');
   error.status=response.status;
   throw error;
  }
  const quote=await response.json();
  if(quote.status!=='READY'||quote.documents?.length!==documents.length||
   !Number.isSafeInteger(quote.total_price_paise)||quote.total_price_paise<=0){
   throw new Error('Cloud batch quote response is incomplete');
  }
  return quote;
 }
 const api={quotePreparedBatch};
 if(typeof module!=='undefined'&&module.exports){module.exports=api;}
 if(root){root.XeroxGoCloudBatchQuote=api;}
})(typeof window!=='undefined'?window:null);
