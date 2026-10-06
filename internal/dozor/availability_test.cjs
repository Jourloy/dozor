const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const context = {window: {}};
vm.runInNewContext(fs.readFileSync(__dirname + '/web/availability.js', 'utf8'), context);
const {intervals, at} = context.window.DozorAvailability;

test('a rolling day clips old intervals and distinguishes outages from missing observations', () => {
  const rows = intervals({start: 100, end: 1000}, {intervals: [
    {start: 0, end: 200, state: 'online'},
    {start: 200, end: 300, state: 'offline'},
    {start: 500, end: 1200, state: 'online'},
  ]});
  assert.deepEqual(JSON.parse(JSON.stringify(rows)), [
    {start: 100, end: 200, state: 'online'},
    {start: 200, end: 300, state: 'offline'},
    {start: 300, end: 500, state: 'unknown'},
    {start: 500, end: 1000, state: 'online'},
  ]);
  assert.equal(at(rows, 200).state, 'offline');
  assert.equal(at(rows, 300).state, 'unknown');
  assert.equal(at(rows, 500).state, 'online');
  assert.equal(at(rows, 1000).state, 'online');
});

test('no history is unknown rather than offline and cameras use the same time axis', () => {
  const history = {start: 100, end: 1000};
  const unknown = intervals(history, {intervals: []});
  const offline = intervals(history, {intervals: [{start: 100, end: 1000, state: 'offline'}]});
  assert.equal(unknown.length, 1);
  assert.equal(at(unknown, 500).state, 'unknown');
  assert.equal(at(offline, 500).state, 'offline');
});
