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
#include "data/constructs/Mutation.h"
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

  interconnect::MasterConnect *master = new MasterConnect(&creds, std::shared_ptr<ZookeeperInstance>(instPtr));

  con->masterPtr = master;

  return con;
}

int free_connector(struct connector *connector) {
  //ZookeeperInstance *instPtr = static_cast<ZookeeperInstance*>(connector->zk);
  //delete instPtr; not needed since we're passing it as a shared ptr to masterconnect
  interconnect::MasterConnect *master = static_cast<interconnect::MasterConnect*>(connector->masterPtr);
  delete master;
  delete connector;
  return 0;
}

TableOps *open_table(struct connector *connector, char *tableName) {
  struct TableOps *tableOps = new TableOps();

  interconnect::MasterConnect *master = static_cast<interconnect::MasterConnect*>(connector->masterPtr);

  AccumuloTableOperations *ops = master->tableOps(tableName).release();

  tableOps->tableOpsPtr = ops;
  tableOps->table_name = tableName;
  tableOps->parent = connector;

  return tableOps;
}

struct TableOps *create_table(struct connector *connector, char *tableName) {
  struct TableOps *tableOps = open_table(connector, tableName);

  AccumuloTableOperations *tableOpsCpp = static_cast<AccumuloTableOperations*>(tableOps->tableOpsPtr);

  tableOpsCpp->create(false);

  return tableOps;
}

int drop_table(struct TableOps *tableOps){
  if (NULL!= tableOps && NULL != tableOps->tableOpsPtr) {
      AccumuloTableOperations *tableOpsCpp = static_cast<AccumuloTableOperations*>(tableOps->tableOpsPtr);
      if (tableOpsCpp->exists())
        tableOpsCpp->remove();
      delete tableOpsCpp;
      delete tableOps;
    }
    return 1;

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
    cclient::data::security::Authorizations *auths = new cclient::data::security::Authorizations();
    scanner->scannerPtr = tableOpsCpp->createScanner(auths, threads).release();
    return scanner;
  }
  return 0;
}

struct CMutation *createMutation(char *row){
    if (row != nullptr){
        cclient::data::Mutation *m = new cclient::data::Mutation(row);
        struct CMutation *nm = new CMutation();
        nm->mutationPtr = m;
        return nm;
    }
    return nullptr;
}


void freeMutation(struct CMutation *mutation){
    if (mutation != nullptr && mutation->mutationPtr != nullptr){
        cclient::data::Mutation *m = static_cast<cclient::data::Mutation*>(mutation->mutationPtr);
        delete m;
        delete mutation;
    }
}

void put(struct CMutation *mutation, char *cf, char *cq, char *cv){
    if (mutation != nullptr){
        cclient::data::Mutation *m = static_cast<cclient::data::Mutation*>(mutation->mutationPtr);
        m->put(cf,cq,cv);
    }
}


struct BatchWriter *createWriter(struct TableOps *tableOps, short threads){
  if (NULL != tableOps->tableOpsPtr) {
    AccumuloTableOperations *tableOpsCpp = static_cast<AccumuloTableOperations*>(tableOps->tableOpsPtr);
    struct BatchWriter *writer = new BatchWriter();
    cclient::data::security::Authorizations auths;
    writer->writerPtr = tableOpsCpp->createWriter(&auths, threads).release();
    return writer;
  }
  return 0;
}

int addMutation(struct BatchWriter *writer, struct CMutation *mutation){
    if (nullptr != writer && nullptr != mutation){
        writer::Sink<cclient::data::KeyValue> *client_writer = static_cast<writer::Sink<cclient::data::KeyValue>*>(writer->writerPtr);
        cclient::data::Mutation *m = static_cast<cclient::data::Mutation*>(mutation->mutationPtr);
        client_writer->addMutation(std::unique_ptr<cclient::data::Mutation>(m));
        // avoid frees if user calls freeMutation
        mutation->mutationPtr = nullptr;
        return 0;
    }
    return 1;
}



void populateKey(CKey *key, const std::shared_ptr<cclient::data::Key> &otherKey) {
  std::pair<char*, size_t> row = otherKey->getRow();
  key->row = new char[row.second];
    //memcpy(key->row, row.first, row.second);
  strcpy(key->row,row.first);

  std::pair<char*, size_t> cf = otherKey->getColFamily();
  key->colFamily = new char[cf.second];
  //memcpy(key->colFamily, cf.first, cf.second);
    strcpy(key->colFamily,cf.first);

  std::pair<char*, size_t> cq = otherKey->getColQualifier();
  key->colQualifier = new char[cq.second];
  if (cq.second > 0)
      strcpy(key->colQualifier,cq.first);
    //memcpy(key->colQualifier, cq.first, cq.second);
  else
      key->colQualifier[0] = 0x00;

  std::pair<char*, size_t> cv = otherKey->getColVisibility();
  key->keyVisibility = new char[cv.second];
  if (cv.second > 0)
      strcpy(key->keyVisibility,cv.first);
  //memcpy(key->keyVisibility, cv.first, cv.second);
  else
      key->keyVisibility[0] = 0x00;


  key->timestamp = otherKey->getTimeStamp();
}

void populateKeyValue(CKeyValue *kv, const std::shared_ptr<cclient::data::KeyValue> &otherKv) {
  populateKey(kv->key, otherKv->getKey());
  cclient::data::Value *val = otherKv->getValue().get();
  kv->value->value = new uint8_t[val->size()];
  memcpy(kv->value->value, val->data(), val->size());
}

std::shared_ptr<cclient::data::Key> toKey(CKey *key) {
  auto nk = std::make_shared<cclient::data::Key>();
  if (strlen(key->row) > 0)
    nk->setRow((const char*) key->row, strlen(key->row));
  if (strlen(key->colFamily) > 0)
    nk->setColFamily((const char*) key->colFamily, strlen(key->colFamily) );
  if (strlen(key->colQualifier) > 0)
    nk->setColQualifier((const char*) key->colQualifier, strlen(key->colQualifier));
  if (strlen(key->keyVisibility) > 0)
    nk->setColVisibility((const char*) key->keyVisibility, strlen(key->keyVisibility));

  nk->setTimeStamp(key->timestamp);
  //nk->setDeleted(key->deleted);
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
  bool hasNext = false;
  if (scanner->res == 0) {

      auto res  = new ScanRes(bs);
      hasNext = res->hasNext();

              scanner->res = res;
  }else{
      ScanRes *res = static_cast<ScanRes*>(scanner->res);
      hasNext= res->hasNext();
  }

  return hasNext;
}


CKeyValue *next(struct BatchScan *scanner) {
    //scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
    ScanRes *res = static_cast<ScanRes*>(scanner->res);


    CKeyValue *kv = new CKeyValue();
    kv->key = new CKey();
    populateKey(kv->key,res->next());

    return kv;
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

/**
 *
 * @param row
 * @param cf
 * @param cq
 * @param cv
 * @param timestamp
 * @return
 */
struct CKey *createKey(char *row, char *cf, char *cq, char *cv, uint64_t timestamp){
    if (row == nullptr) {
        return nullptr;
    }

   struct CKey *key = new CKey();
   key-> row = new char[strlen(row)+1];
   memcpy(key->row,row,strlen(row));

   if (cf != nullptr) {
       key->colFamily = new char[strlen(cf)+1];
       memcpy(key->colFamily,cf,strlen(cf));
   }

   if (cq != nullptr) {
       key->colQualifier = new char[strlen(cq)+1];
       memcpy(key->colQualifier,cq,strlen(cq));
   }

    if (cv != nullptr) {
        key->keyVisibility = new char[strlen(cv)+1];
        memcpy(key->keyVisibility,cv,strlen(cv));
    }

    key->timestamp = timestamp;

    return key;
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

int closeWriter(struct BatchWriter *writer) {
    if (0 != writer && 0 != writer->writerPtr) {
        writer::Sink<cclient::data::KeyValue> *client_writer = static_cast<writer::Sink<cclient::data::KeyValue>*>(writer->writerPtr);
        client_writer->close();

        writer->writerPtr = nullptr;
        delete client_writer;
        delete writer;
        return 0;
    } else {
        return 1;
    }
}


}

