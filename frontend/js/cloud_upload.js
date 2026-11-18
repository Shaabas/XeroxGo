/**
 * XG-CLOUD-DOCS-V2: opt-in direct-to-Supabase upload transport.
 * Not wired into the production cart until prepared-file checkout is ready.
 * No service-role key or credentials belong in this browser module.
 */
(function (root) {
  'use strict';

  function uploadSignedFile(signedURL, file, onProgress, xhrFactory) {
    return new Promise((resolve, reject) => {
      const xhr = xhrFactory();
      xhr.open('PUT', signedURL);
      xhr.setRequestHeader('Content-Type', 'application/pdf');
      xhr.upload.addEventListener('progress', event => {
        if (event.lengthComputable && event.total > 0) {
          onProgress(Math.min(100, Math.round(event.loaded / event.total * 100)));
        }
      });
      xhr.addEventListener('load', () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve();
        } else {
          reject(new Error('Cloud upload rejected (' + xhr.status + ')'));
        }
      });
      xhr.addEventListener('error', () => reject(new Error(
        'Cloud connection interrupted; check the upload state before retrying.'
      )));
      xhr.addEventListener('abort', () => reject(new Error('Cloud upload canceled')));
      xhr.send(file);
    });
  }

  async function cloudUploadPDF(file, kioskID, onProgress = () => {},
                                request = fetch, xhrFactory = () => new XMLHttpRequest()) {
    if (!file || !file.name || file.type !== 'application/pdf' ||
        file.size < 1 || file.size > 25 * 1024 * 1024) {
      throw new Error('Select a PDF of 25 MiB or less');
    }
    const reply = await request('/api/documents/uploads/init', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        kiosk_id: kioskID,
        filename: file.name,
        content_type: 'application/pdf',
        size_bytes: file.size
      })
    });
    if (!reply.ok) throw new Error('Cloud upload authorization unavailable');
    const grant = await reply.json();
    if (!grant.upload_url || !grant.upload_id || !grant.upload_token) {
      throw new Error('Invalid cloud upload grant');
    }

    // Retain grant in memory so a failed finalize can be recovered without
    // uploading bytes again. Never persist it in URLs, logs, or localStorage.
    const receipt = { upload_id: grant.upload_id, upload_token: grant.upload_token };
    await uploadSignedFile(grant.upload_url, file, onProgress, xhrFactory);
    const completed = await request('/api/documents/uploads/complete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(receipt)
    });
    if (!completed.ok) {
      const err = new Error('Uploaded PDF could not be verified by the cloud');
      err.uploadReceipt = receipt;
      throw err;
    }
    const result = await completed.json();
    if (result.status !== 'UPLOADED' || result.upload_id !== receipt.upload_id) {
      throw new Error('Unexpected cloud upload verification result');
    }
    return { uploadId: result.upload_id, checksum: result.sha256 };
  }

  const api = { uploadSignedFile, cloudUploadPDF };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.XeroxGoCloudUpload = api;
})(typeof window !== 'undefined' ? window : null);
