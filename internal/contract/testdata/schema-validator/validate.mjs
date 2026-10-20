import fs from 'node:fs';
import Ajv2020 from 'ajv/dist/2020.js';
const ajv = new Ajv2020({strict: false, allErrors: true, unicodeRegExp: true, validateFormats: false});
const cases = JSON.parse(fs.readFileSync(0, 'utf8'));
const results = cases.map(({schema, value}) => {
  const validate = ajv.compile(schema);
  const valid = validate(value);
  return {valid, errors: validate.errors};
});
process.stdout.write(JSON.stringify(results));
