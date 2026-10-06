// Actual pinned browser and generated client requests, recorded before forwarding.
import http from 'node:http';
import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {createRequire} from 'node:module';
import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {chromium} from 'playwright-core';
const require=createRequire(import.meta.url);
const gateway=process.argv[2];
const dir=process.env.P10_EVIDENCE_DIR || await fs.mkdtemp(path.join(os.tmpdir(),'p10-wire-'));
await fs.mkdir(dir,{recursive:true});
const jar=process.env.P10_GENERATOR_JAR;
if (!jar || createHash('sha256').update(await fs.readFile(jar)).digest('hex')!=='41ce4f6b07f196676439d710759fa1ced7a08066d06ff1bf314681470289efae') throw Error('pinned OpenAPI Generator 7.25.0 JAR required');
let document=await (await fetch(gateway+'/openapi.json',{headers:{Authorization:'Bearer p10-key'}})).json();
let recordings=[];
const uiRoot=path.dirname(require.resolve('swagger-ui-dist/package.json'));
const server=http.createServer(async(req,res)=>{try{
 if(req.url==='/'){res.setHeader('Content-Type','text/html; charset=utf-8');res.end(`<div id="swagger-ui"></div><link rel="stylesheet" href="/swagger-ui.css"><script src="/swagger-ui-bundle.js"></script><script>window.ui=SwaggerUIBundle({url:'/openapi.json',dom_id:'#swagger-ui',onComplete:()=>ui.preauthorizeApiKey('bearerAuth','p10-key')});</script>`);return;}
 if(req.url==='/swagger-ui.css'||req.url==='/swagger-ui-bundle.js'){res.setHeader('Content-Type',req.url.endsWith('.js')?'application/javascript; charset=utf-8':'text/css; charset=utf-8');res.end(await fs.readFile(path.join(uiRoot,req.url.slice(1))));return;}
 if(req.url==='/openapi.json'){res.setHeader('Content-Type','application/json');res.end(JSON.stringify(document));return;}
 const chunks=[];let size=0;for await(const chunk of req){size+=chunk.length;if(size>2<<20)throw Error('recording limit');chunks.push(chunk);}const body=Buffer.concat(chunks);
 const record={method:req.method,url:req.url,headers:req.headers,body_base64:body.toString('base64')}; recordings.push(record);
 const target=new URL(req.url,gateway);const forwarded=http.request(target,{method:req.method,headers:{...req.headers,host:target.host,authorization:'Bearer p10-key'}},up=>{record.status=up.statusCode;const responseChunks=[];up.on('data',chunk=>responseChunks.push(chunk));up.on('end',()=>record.response_base64=Buffer.concat(responseChunks).toString('base64'));res.writeHead(up.statusCode,up.headers);up.pipe(res);});forwarded.on('error',e=>{res.statusCode=502;res.end(String(e));});forwarded.end(body);
 }catch(e){res.statusCode=500;res.end(String(e));}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));const base='http://127.0.0.1:'+server.address().port;
document.servers=[{url:base}];
await fs.writeFile(path.join(dir,'openapi.json'),JSON.stringify(document,null,2));
let browser;let page;
try{
 const generated=path.join(dir,'generated');const run=(cmd,args)=>{let r=spawnSync(cmd,args,{encoding:'utf8'});if(r.status!==0)throw Error(`${cmd}: ${r.stdout}\n${r.stderr}`);return r.stdout;};
 await fs.writeFile(path.join(dir,'generator.log'),run('java',['-jar',jar,'generate','-i',path.join(dir,'openapi.json'),'-g','typescript-fetch','-o',generated,'--additional-properties=withoutRuntimeChecks=true','--type-mappings=null=null,Null=null','--global-property=skipFormModel=false']));
 await fs.writeFile(path.join(dir,'generated-tsc.log'),run(process.execPath,[require.resolve('typescript/bin/tsc'),'--target','es2022','--module','commonjs','--lib','es2022,dom','--outDir',path.join(generated,'compiled'),path.join(generated,'index.ts')]));
 const api=require(path.join(generated,'compiled','index.js'));
 const configuration=new api.Configuration({basePath:base,accessToken:'p10-key'});
 const client=new api.DefaultApi(configuration);
 const apiSource=await fs.readFile(path.join(generated,'apis','DefaultApi.ts'),'utf8');const parameter=apiSource.match(/async save(?:Raw|RequestOpts)[\s\S]*?requestParameters\['([^']+)'\]/)[1];
 const response=await client.save({[parameter]:{document:{name:'report.bin',mime_type:'application/octet-stream',data:Buffer.from([0,255,1,2,3]).toString('base64')},number:42,nested:{},label:'x'}});
 if(!response.result || response.result.files.length!==1)throw Error('generated client failed');
 const generatedRecord=recordings.at(-1);if(generatedRecord.status!==200)throw Error('generated HTTP status '+generatedRecord.status);
 await fs.writeFile(path.join(dir,'report.bin'),Buffer.from([0,255,1,2,3]));
 browser=await chromium.launch({headless:true,executablePath:process.env.P10_BROWSER || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',args:['--no-sandbox']});
 console.log("browser",browser.version());page=await browser.newPage();page.on("pageerror",e=>console.error("browser error",e.stack));page.on("console",m=>{if(m.type()==="error")console.error("browser console",m.text())});page.setDefaultTimeout(8000);await page.goto(base);await fs.writeFile(path.join(dir,"swagger-dom.html"),await page.content());const operation=page.locator('#operations-default-save');await operation.locator('.opblock-summary').click();await operation.getByRole('button',{name:'Try it out',exact:true}).click();
 // The operation supports JSON and multipart; select actual multipart rendering.
 await operation.locator('select').filter({has:page.locator('option[value="multipart/form-data"]')}).first().selectOption('multipart/form-data');
 await operation.locator('input[type=file]').first().setInputFiles({name:'report.bin',mimeType:'application/octet-stream',buffer:Buffer.from([0,255,1,2,3])});
 await operation.locator('[data-property-name=label] input[type=text]').fill('\"x\"');await operation.locator('[data-property-name=number] input[type=text]').fill('42');await operation.locator('[data-property-name=nested] textarea').fill('{}');
 await operation.getByRole('button',{name:'Execute',exact:true}).click();await operation.locator('.response-col_status').filter({hasText:'200'}).first().waitFor({timeout:20000});
 await page.screenshot({path:path.join(dir,'swagger-ui.png'),fullPage:true});
 const stringRecord=recordings.at(-1);if(stringRecord.status!==400)throw Error('Swagger string limitation was not detected');stringRecord.case='swagger_JSON_string_serializes_plain_bytes_unsupported';
 await operation.locator('[data-property-name=label] input[type=text]').fill(JSON.stringify(JSON.stringify('x')));await Promise.all([page.waitForResponse(r=>r.url().endsWith('/capabilities/save')&&r.request().method()==='POST',{timeout:8000}),operation.getByRole('button',{name:'Execute',exact:true}).click()]);const swaggerRecord=recordings.at(-1);if(swaggerRecord.status!==200)throw Error('Swagger supported HTTP status '+swaggerRecord.status);

 // Compare real generated JSON with real browser multipart for byte identity.
 const parameterFor=method=>apiSource.match(new RegExp('async '+method+'(?:Raw|RequestOpts)[\\s\\S]*?requestParameters\\[\'([^\']+)\'\\]'))[1];
 const generatedCall=async(method,value)=>client[method]({[parameterFor(method)]:value});
 const file=(name,body,mime='application/octet-stream')=>({name,mime_type:mime,data:Buffer.from(body).toString('base64')});
 const compare=()=>{const success=recordings.filter(r=>r.status===200&&r.url.startsWith('/api/v1/capabilities/'));const envelopes=success.map(r=>JSON.parse(Buffer.from(r.response_base64,'base64').toString()));return envelopes;};
 await generatedCall('array',{documents:[file('b.bin',[3,4]),file('a.bin',[5])]});
 const arrayOperation=page.locator('#operations-default-array');await arrayOperation.locator('.opblock-summary').click();await arrayOperation.getByRole('button',{name:'Try it out',exact:true}).click();await arrayOperation.locator('select').filter({has:page.locator('option[value="multipart/form-data"]')}).first().selectOption('multipart/form-data');
 await arrayOperation.locator('input[type=file]').first().setInputFiles({name:'b.bin',mimeType:'application/octet-stream',buffer:Buffer.from([3,4])});await arrayOperation.getByRole('button',{name:'Add string item',exact:true}).click();await arrayOperation.locator('input[type=file]').nth(1).setInputFiles({name:'a.bin',mimeType:'application/octet-stream',buffer:Buffer.from([5])});await Promise.all([page.waitForResponse(r=>r.url().endsWith('/capabilities/array')&&r.request().method()==='POST',{timeout:8000}),arrayOperation.getByRole('button',{name:'Execute',exact:true}).click()]);if(recordings.at(-1).status!==200)throw Error('Swagger repeated binary parts failed');
 const arrayEnvelopes=compare().filter(e=>e.result.files.length===2).map(e=>e.result.files.map(f=>({name:f.name,bytes:f.size_bytes,sha256:f.sha256,mime:f.mime_type})));if(JSON.stringify(arrayEnvelopes[0])!==JSON.stringify(arrayEnvelopes[1]))throw Error('array byte/order metadata differ');
 for (const body of [[],[123,98,105,110,97,114,121]]) {
  await generatedCall('save',{document:file('report.bin',body,'application/json'),number:9007199254740991,nested:{file:file('nested.bin',[7])},label:'x'});
  await operation.locator('input[type=file]').first().setInputFiles({name:'report.bin',mimeType:'application/json',buffer:Buffer.from(body)});await operation.locator('[data-property-name=number] input[type=text]').fill('9007199254740991');await operation.locator('[data-property-name=nested] textarea').fill(JSON.stringify({file:file('nested.bin',[7])}));await Promise.all([page.waitForResponse(r=>r.url().endsWith('/capabilities/save')&&r.request().method()==='POST',{timeout:8000}),operation.getByRole('button',{name:'Execute',exact:true}).click()]);if(recordings.at(-1).status!==200)throw Error('Swagger JSON MIME/nested/empty failed');
 }
 await generatedCall('nested',{nested:{file:file('nested.bin',[7,8,9])}});
 const nestedOperation=page.locator('#operations-default-nested');await nestedOperation.locator('.opblock-summary').click();await nestedOperation.getByRole('button',{name:'Try it out',exact:true}).click();await nestedOperation.locator('select').filter({has:page.locator('option[value="multipart/form-data"]')}).first().selectOption('multipart/form-data');await nestedOperation.locator('[data-property-name=nested] textarea').fill(JSON.stringify({file:file('nested.bin',[7,8,9])}));await Promise.all([page.waitForResponse(r=>r.url().endsWith('/capabilities/nested')&&r.request().method()==='POST',{timeout:8000}),nestedOperation.getByRole('button',{name:'Execute',exact:true}).click()]);if(recordings.at(-1).status!==200)throw Error('Swagger nested save failed');
 const nestedEnvelopes=recordings.filter(r=>r.url.endsWith('/capabilities/nested')&&r.status===200).map(r=>JSON.parse(Buffer.from(r.response_base64,'base64').toString()).result.files.map(f=>({name:f.name,bytes:f.size_bytes,sha256:f.sha256,mime:f.mime_type})));if(JSON.stringify(nestedEnvelopes[0])!==JSON.stringify(nestedEnvelopes[1]))throw Error('nested saved bytes differ');
 const successful=recordings.filter(r=>r.status===200&&r.url.endsWith('/capabilities/save'));for(let i=0;i<successful.length;i+=2){const info=r=>JSON.parse(Buffer.from(r.response_base64,'base64').toString()).result.files.map(f=>({name:f.name,bytes:f.size_bytes,sha256:f.sha256,mime:f.mime_type}));if(JSON.stringify(info(successful[i]))!==JSON.stringify(info(successful[i+1])))throw Error('JSON/multipart byte metadata differ');}
 // JavaScript cannot represent the complete int64 domain. Prove rejection of
 // its rounded maximum instead of claiming this input passed compatibility.
 try{await generatedCall('save',{document:file('report.bin',[1]),number:Number(9223372036854775807n)});throw Error('rounded int64 was accepted');}catch(e){if(!e.response||e.response.status!==400)throw e;}
 await fs.writeFile(path.join(dir,'swagger-ui.png'),await page.screenshot({fullPage:true}));
 await fs.writeFile(path.join(dir,'wire.json'),JSON.stringify({versions:{swagger_ui:'5.27.1',openapi_generator:'7.25.0',typescript:'5.9.3',playwright:'1.56.1'},recordings},null,2));
 console.log(JSON.stringify({evidence:dir,requests:recordings.map(r=>({url:r.url,status:r.status,bytes:Buffer.from(r.body_base64,'base64').length}))}));
}catch(e){if(page){await page.screenshot({path:path.join(dir,'failure.png'),fullPage:true});await fs.writeFile(path.join(dir,'failure-dom.html'),await page.content());}throw e;}finally{await fs.writeFile(path.join(dir,'wire.json'),JSON.stringify({versions:{swagger_ui:'5.27.1',openapi_generator:'7.25.0',typescript:'5.9.3',playwright:'1.56.1',browser:browser?.version()},generator_settings:{withoutRuntimeChecks:true,type_mappings:'null=null,Null=null',skipFormModel:false},recordings},null,2));if(browser)await browser.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
