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
using namespace std;
#include "data/constructs/Key.h";
#include "data/constructs/KeyValue.h"
#include "data/constructs/security/Authorizations.h"
#include "scanner/constructs/Results.h"
#include "scanner/impl/Scanner.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "interconnect/Master.h"
#include "interconnect/tableOps/TableOperations.h"

#include "extern/accumulo.h"

using namespace cclient::data;
using namespace cclient::data::zookeeper;
using namespace cclient::data::streams;
using namespace interconnect;
using namespace scanners;

extern "C"
{

  struct connector *
  create_connector (char *instance, char *zks, char *username, char *password)
  {
    Configuration conf;
    conf.set ("FILE_SYSTEM_ROOT", "/accumulo");
    ZookeeperInstance *instPtr = new ZookeeperInstance (string (instance),
							string (zks), 1000,
							&conf);

    struct connector *con = new connector ();

    con->zk = instPtr;

    AuthInfo creds (string (username), string (password),
		    instPtr->getInstanceId ());

    interconnect::MasterConnect *master = new MasterConnect (&creds, instPtr);

    con->masterPtr = master;

    return con;
  }

  int
  free_connector (struct connector *connector)
  {
    ZookeeperInstance *instPtr = static_cast<ZookeeperInstance*> (connector->zk);
    delete instPtr;
    interconnect::MasterConnect *master =
	static_cast<interconnect::MasterConnect*> (connector->masterPtr);

    delete master;
    delete connector;
    return 0;
  }

  TableOps *
  open_table (struct connector *connector, char *tableName)
  {
    struct TableOps *tableOps = new TableOps ();

    interconnect::MasterConnect *master =
	static_cast<interconnect::MasterConnect*> (connector->masterPtr);

    TableOperations<KeyValue*, ResultBlock<KeyValue*>> *ops = master->tableOps (
	tableName);

    tableOps->tableOpsPtr = ops;
    tableOps->table_name = tableName;

    return tableOps;
  }

  struct TableOps *
  create_table (struct connector *connector, char *tableName)
  {
    struct TableOps *tableOps = open_table (connector, tableName);

    AccumuloTableOperations *tableOpsCpp =
	static_cast<AccumuloTableOperations*> (tableOps->tableOpsPtr);

    tableOpsCpp->create (true);

    return tableOps;
  }
  int
  free_table (struct TableOps *tableOps)
  {
    if (NULL != tableOps->tableOpsPtr)
      {
	AccumuloTableOperations *tableOpsCpp =
	    static_cast<AccumuloTableOperations*> (tableOps->tableOpsPtr);

	delete tableOps;
      }
    delete tableOps;
    return 1;
  }
  
  struct Scanner *createScanner(struct TableOps *tableOps, short threads){
    if (NULL != tableOps->tableOpsPtr)
      {
    AccumuloTableOperations *tableOpsCpp =
	    static_cast<AccumuloTableOperations*> (tableOps->tableOpsPtr);
	   struct Scanner *scanner = new Scanner();
	   
	   scanner.scannerPtr= tableOpsCpp->createScanner(tableOps->table_name, threads).release();
	   return scanner;
      }
      return 0;
  }

cclient::data::Key *toKey(Key *key)
{
  cclient::data::Key *nk = new cclient::data::Key();
  nk->setRow(key->row,key->rowLength);
  nk->setColFamily(key->colFamily,key->columnFamilyLength);
  nk->setColQualifier(key->colQualifier,key->colQualLen);
  nk->setColVisibility(key->colVisSize,key->colVisSize);
  nk->setTimeStamp(key->timestamp);
  nk->setDeleted(key->deleted);
  return nk;
}

cclient::data::Range *toRange(Range *range)
{
  cclient::data::Range *nr = new cclient::data::Range(toKey(range->start),range->startKeyInclusive,toKey(range->stop),range->stopKeyInclusive);
  return nr;
}
  
int addRange(struct Scanner *scanner, Range *range)
{
  scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
  cclient::data::Range *nr = toRange(range);
  bs->addRange(std::unique_ptr(nr);
  return 1;
}

int addRanges(struct Scanner *scanner, Range **range, int size)
{
  scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
  int i = 0;
  for(i=0; i < size; i++)
  {
    cclient::data::Range *nr = toRange(range[i]);
    bs->addRange(std::unique_ptr(nr);
  }
  return i;
}

bool hasNext(struct Scanner *scanner)
{
  scanners::BatchScanner *bs = static_cast<scanners::BatchScanner*>(scanner->scannerPtr);
  if (scanner->res == 0)
    scanner->res=bs->getResultSet();
  return ((scanners::Results<cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>*)scanner->res);
}

int next(struct Scanner *scanner, KeyValue *kv);

int next(struct Scanner *scanner, KeyValueList *kvl);

int closeScanner(struct Scanner *scanner)
{
  delete scanner.scannerPtr;
  delete scanner;
}

}


