const test = require('node:test');
const assert = require('node:assert/strict');
const { confirmedOrderQuote } = require('./portal.js');

const tariff = { revision: 7 };
function item(price, revision = 7) {
  return { status: 'ready', authoritativePricePaise: price, tariffRevision: revision,
    quotePending: false, quoteError: '' };
}

test('checkout accepts previously confirmed multi-file quotes without re-quoting', () => {
  assert.deepEqual(confirmedOrderQuote([item(300), item(800), item(500)], tariff),
    { revision: 7, acceptedTotal: 1600 });
});

test('checkout requires ready, positive, current-revision cloud quotes', () => {
  assert.equal(confirmedOrderQuote([], tariff), null);
  assert.equal(confirmedOrderQuote([item(300)], null), null);
  assert.equal(confirmedOrderQuote([item(300)], { revision: 0 }), null);
  assert.equal(confirmedOrderQuote([item(300, 6)], tariff), null);
  assert.equal(confirmedOrderQuote([item(0)], tariff), null);
  assert.equal(confirmedOrderQuote([item(NaN)], tariff), null);
  assert.equal(confirmedOrderQuote([item(12.5)], tariff), null);
  assert.equal(confirmedOrderQuote([item(300), { ...item(500), quotePending: true }], tariff), null);
  assert.equal(confirmedOrderQuote([item(300), { ...item(500), quoteError: 'quote failed' }], tariff), null);
  assert.equal(confirmedOrderQuote([{ ...item(300), status: 'processing' }], tariff), null);
});

test('checkout rejects integer overflow rather than rounding the accepted price', () => {
  assert.equal(confirmedOrderQuote([item(Number.MAX_SAFE_INTEGER), item(1)], tariff), null);
});
