'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const { deriveCustomerOrderStatus, shouldStopOrderPolling } = require('./portal.js');

test('first child complete is not whole-order completion', () => {
  const response = { status: 'COMPLETED', documents_total: 3, documents_completed: 1 };
  assert.equal(deriveCustomerOrderStatus(response).status, 'PRINTING');
  assert.equal(shouldStopOrderPolling(response), false);
});

test('all three child documents complete permits final stop', () => {
  const response = { status: 'COMPLETED', documents_total: 3, documents_completed: 3 };
  assert.equal(deriveCustomerOrderStatus(response).status, 'COMPLETED');
  assert.equal(shouldStopOrderPolling(response), true);
});

test('invalid completion count cannot suppress ongoing polling', () => {
  assert.equal(shouldStopOrderPolling({
    status: 'COMPLETED', documents_total: 3, documents_completed: 4
  }), false);
  assert.equal(shouldStopOrderPolling({
    status: 'COMPLETED', documents_total: 3, documents_completed: 0
  }), false);
});

test('unresolved additional payment refund keeps polling after all printing', () => {
  assert.equal(shouldStopOrderPolling({
    status: 'COMPLETED', documents_total: 2, documents_completed: 2,
    excess_refunds_unresolved: 1
  }), false);
});

test('legacy single-job completion remains compatible', () => {
  assert.equal(shouldStopOrderPolling({ status: 'COMPLETED' }), true);
  assert.equal(shouldStopOrderPolling({ status: 'PRINTING' }), false);
});

test('refund and printer-fault statuses are never rewritten as completion', () => {
  for (const status of ['FAILED_REFUNDABLE', 'PAUSED_ERROR', 'REFUND_PENDING', 'REFUNDED']) {
    const payload = { status, documents_total: 3, documents_completed: 1 };
    assert.equal(deriveCustomerOrderStatus(payload).status, status);
    assert.equal(shouldStopOrderPolling(payload), false);
  }
});
