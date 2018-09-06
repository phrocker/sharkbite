/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include <iostream>
#include <sstream>
#include "data/constructs/Key.h"
#include "data/constructs/KeyValue.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "data/constructs/configuration/Configuration.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Master.h"
#include "interconnect/tableOps/TableOperations.h"

using namespace cclient::data;
using namespace cclient::data::zookeeper;
using namespace cclient::data::streams;
using namespace interconnect;
using namespace scanners;

#include "extern/accumulo.h"
#include "extern/accumulo_data.h"

extern "C" {

struct connector *
create_connector(char *instance, char *zks, char *username, char *password) {
  ZookeeperInstance *instPtr = new ZookeeperInstance(string(instance), string(zks), 1000);

  struct connector *con = new connector();

  con->zk = instPtr;

  cclient::data::security::AuthInfo creds(string(username), string(password), instPtr->getInstanceId());

  interconnect::MasterConnect *master = new MasterConnect(&creds, instPtr);

  con->masterPtr = master;

  return con;
}

int free_connector(struct connector *connector) {
  ZookeeperInstance *instPtr = static_cast<ZookeeperInstance*>(connector->zk);
  delete instPtr;
  interconnect::MasterConnect *master = static_cast<interconnect::MasterConnect*>(connector->masterPtr);
  delete master;
  delete connector;
  return 0;
}

TableOps *
open_table(struct connector *connector, char *tableName) {
  struct TableOps *tableOps = new TableOps();

  interconnect::MasterConnect *master = static_cast<interconnect::MasterConnect*>(connector->masterPtr);

  AccumuloTableOperations *ops = master->tableOps(tableName).release();

  tableOps->tableOpsPtr = ops;
  tableOps->table_name = tableName;

  return tableOps;
}

struct TableOps *
create_table(struct connector *connector, char *tableName) {
  struct TableOps *tableOps = open_table(connector, tableName);

  AccumuloTableOperations *tableOpsCpp = static_cast<AccumuloTableOperations*>(tableOps->tableOpsPtr);

  tableOpsCpp->create(true);

  return tableOps;
}

int free_table(struct TableOps *tableOps) {
  if (NULL != tableOps->tableOpsPtr) {
    AccumuloTableOperations *tableOpsCpp = static_cast<AccumuloTableOperations*>(tableOps->tableOpsPtr);
    delete tableOpsCpp;
    delete tableOps;
  }
  delete tableOps;
  return 1;
}

struct BatchScan *createScanner(struct TableOps *tableOps, short threads) {
  if (NULL != tableOps->tableOpsPtr) {
    AccumuloTableOperations *tableOpsCpp = static_cast<AccumuloTableOperations*>(tableOps->tableOpsPtr);
    struct BatchScan *scanner = new BatchScan();
    cclient::data::security::Authorizations auths;
    scanner->scannerPtr = tableOpsCpp->createScanner(&auths, threads).release();
    return scanner;
  }
  return 0;
}

void populateKey(CKey *key, const std::shared_ptr<cclient::data::Key> &otherKey) {
  std::pair<char*, size_t> row = otherKey->getRow();
  key->row = new uint8_t[row.second];
  key->rowLength = row.second;
  memcpy(key->row, row.first, row.second);

  std::pair<char*, size_t> cf = otherKey->getColFamily();
  key->colFamily = new uint8_t[cf.second];
  key->columnFamilyLength = cf.second;
  memcpy(key->colFamily, cf.first, cf.second);

  std::pair<char*, size_t> cq = otherKey->getColQualifier();
  key->colQualifier = new uint8_t[cq.second];
  key->colQualLen = cq.second;
  memcpy(key->colQualifier, cq.first, cq.second);

  std::pair<char*, size_t> cv = otherKey->getColVisibility();
  key->keyVisibility = new uint8_t[cv.second];
  key->colVisSize = cv.second;
  memcpy(key->keyVisibility, cv.first, cv.second);

  key->timestamp = otherKey->getTimeStamp();
}

void populateKeyValue(CKeyValue *kv, cclient::data::KeyValue *otherKv) {
  populateKey(kv->key, otherKv->getKey());
  cclient::data::Value *val = otherKv->getValue().get();
  kv->value->value = new uint8_t[val->size()];
  memcpy(kv->value->value, val->data(), val->size());
}

std::shared_ptr<cclient::data::Key> toKey(CKey *key) {
  auto nk = std::make_shared<cclient::data::Key>();
  nk->setRow((const char*) key->row, key->rowLength);
  nk->setColFamily((const char*) key->colFamily, key->columnFamilyLength);
  nk->setColQualifier((const char*) key->colQualifier, key->colQualLen);
  nk->setColVisibility((const char*) key->keyVisibility, key->colVisSize);
  nk->setTimeStamp(key->timestamp);
  nk->setDeleted(key->deleted);
  return nk;
}

cclient::data::Range *toRange(CRange *range) {
  cclient::data::Range *nr = new cclient::data::Range(toKey(range->start), range->startKeyInclusive, toKey(range->stop), range->stopKeyInclusive);
  return nr;
}

int addRange(struct BatchScan *scanner, CRange *range) {
  scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
  cclient::data::Range *nr = toRange(range);
  bs->addRange(std::unique_ptr<cclient::data::Range>(nr));
  return 1;
}

int addRanges(struct BatchScan *scanner, CRange **range, int size) {
  scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
  int i = 0;
  for (i = 0; i < size; i++) {
    cclient::data::Range *nr = toRange(range[i]);
    bs->addRange(std::unique_ptr<cclient::data::Range>(nr));
  }
  return i;
}

bool hasNext(struct BatchScan *scanner) {
  scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
  if (scanner->res == 0) {
    scanners::Iterator<cclient::data::KeyValue> *results = bs->getResultSet();
    scanners::Results<KeyValue, ResultBlock<KeyValue>>::iterator rs = results->begin();
    scanner->res = &rs;
  }
  scanners::ResultBlock<cclient::data::KeyValue>* st = static_cast<scanners::Results<KeyValue, ResultBlock<KeyValue>>::iterator*>(scanner->res);
  return !st->isEndOfRange();
}

int next(struct BatchScan *scanner, CKeyValue *kv) {
  //scanners::Results<KeyValue,ResultBlock<KeyValue>>::iterator st = static_cast<scanners::Results<KeyValue,ResultBlock<KeyValue>>::iterator*>(scanner->res);

  //auto nkv = *(*st);

  //auto ptr = nkv.get();
  // populateKeyValue(kv,ptr);
  return 1;
}

int nextMany(struct BatchScan *scanner, KeyValueList *kvl) {
  /*
   scanners::ResultIter<cclient::data::KeyValue>* st = static_cast<scanners::Results<KeyValue,ResultBlock<KeyValue>>::iterator>(scanner->res);
   unsigned int i=0;
   for (i =0; i < kvl->kv_size; i++)
   {
   std::unique_ptr<cclient::data::KeyValue> nkv = *(*st);
   populateKeyValue(kvl->kvs[i],nkv.get());
   }
   return i;*/
  return 1;
}
int closeScanner(struct BatchScan *scanner) {
  if (0 != scanner && 0 != scanner->scannerPtr) {
    scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);

    delete bs;
    delete scanner;
    return 0;
  } else {
    return 1;
  }
}

}

