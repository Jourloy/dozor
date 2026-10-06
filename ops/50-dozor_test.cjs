const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');

test('disk policy allows only Dozor to start an ext4 or exFAT selection unit', () => {
  let rule;
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '50-dozor.rules'), 'utf8'), {
    polkit: {addRule: callback => { rule = callback; }, Result: {YES: 'yes'}},
  });
  const authorize = (unit, user = 'dozor', verb = 'start', id = 'org.freedesktop.systemd1.manage-units') =>
    rule({id, lookup: key => ({unit, verb})[key]}, {user});
  for (const uuid of ['12AB-34CD', '12ab-34cd', '12345678-1234-1234-1234-123456789abc']) {
    const unit = `dozor-disk-prepare@${uuid}.service`;
    assert.equal(authorize(unit), 'yes');
    assert.equal(authorize(unit, 'nobody'), undefined);
    assert.equal(authorize(unit, 'dozor', 'stop'), undefined);
    assert.equal(authorize(unit, 'dozor', 'start', 'org.freedesktop.systemd1.manage-unit-files'), undefined);
  }
  for (const unit of ['ssh.service', 'dozor-disk-prepare@12345678.service', 'dozor-disk-prepare@../12AB-34CD.service', 'dozor-disk-prepare@12AB-34CD.service\n', 'dozor-disk-prepare@12AB-34CD.service.extra']) {
    assert.equal(authorize(unit), undefined, unit);
  }
});
