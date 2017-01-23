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
#include "../../include/extern/../data/constructs/KeyValue.h"
#include "../../include/extern/../data/constructs/security/Authorizations.h"
#include "../../include/extern/../scanner/constructs/Results.h"
#include "../../include/extern/../scanner/impl/Scanner.h"
#include "../../include/extern/../data/constructs/client/zookeeperinstance.h"
#include "../../include/extern/../interconnect/Master.h"
#include "../../include/extern/../interconnect/tableOps/TableOperations.h"

#include "../../include/extern/../data/constructs/rfile/RFile.h"
#include "../../include/extern/../data/constructs/compressor/compressor.h"
#include "../../include/extern/../data/constructs/compressor/zlibCompressor.h"

#include "../../include/extern/../data/streaming/HdfsStream.h"
#include "../../include/extern/accumulo.h"

using namespace cclient::data;
using namespace cclient::data::compression;
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

}

