/**
 * Prepared-document checkout transport for V2.
 * Not wired into the customer cart until full multi-file parity and migration.
 * The caller MUST retain requestId across uncertain-network retries.
 */
(function(root){
 'use strict';
 function isHex(value,length){return typeof value==='string' && new RegExp('^[0-9a-f]{'+length+'}$','i').test(value);}
 function newCheckoutRequestID(randomUUID) {
  const id=randomUUID().replaceAll('-','');
  if(!isHex(id,32)){throw new Error('Secure checkout ID unavailable');}
  return id;
 }
 async function placePreparedOrder(details, request=fetch) {
  if(!details || !isHex(details.uploadId,32) || !isHex(details.uploadToken,64) ||
   !isHex(details.variantId,32) || !isHex(details.requestId,32) ||
   !Number.isSafeInteger(details.pricePaise) || details.pricePaise<=0 ||
   !Number.isSafeInteger(details.tariffRevision) || details.tariffRevision<=0 ||
   !details.kioskId) {throw new Error('Prepared checkout details are incomplete');}
  const response=await request('/api/documents/checkout',{
   method:'POST',headers:{'Content-Type':'application/json'},
   body:JSON.stringify({
    kiosk_id:details.kioskId,upload_id:details.uploadId,
    upload_token:details.uploadToken,variant_id:details.variantId,
    request_id:details.requestId,accepted_total_price_paise:details.pricePaise,
    accepted_tariff_revision:details.tariffRevision
   })
  });
  if(!response.ok){
   const err=new Error(response.status===409
    ? 'Prepared quote changed or the request needs reconciliation.'
    : 'Cloud checkout failed. Confirm whether an order was created before retrying.');
   err.status=response.status;
   err.requestId=details.requestId;
   throw err;
  }
  const order=await response.json();
  if(!order.order_id || !/^\d{4}$/.test(String(order.display_code||''))){
   const err=new Error('Cloud order response is incomplete; check existing order before retrying');
   err.requestId=details.requestId;
   throw err;
  }
  return order;
 }
 const api={newCheckoutRequestID,placePreparedOrder};
 if(typeof module!=='undefined'&&module.exports){module.exports=api;}
 if(root){root.XeroxGoPreparedCheckout=api;}
})(typeof window!=='undefined'?window:null);
