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
#include <algorithm>
#include <sstream>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/writer/impl/SinkImpl.h"
#include "../include/data/constructs/client/zookeeperinstance.h"
#include "../include/interconnect/Master.h"
#include "../include/interconnect/tableOps/TableOperations.h"
#include "../include/interconnect/securityOps/SecurityOperations.h"

#include "../include/data/constructs/rfile/RFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"

#include "../include/data/streaming/HdfsStream.h"

#include <assert.h>

#include "test_base.h"

#define BOOST_IOSTREAMS_NO_LIB 1

int
main (int argc, char **argv)
{

	if (argc < 5) {
		std::cout << "Arguments required: ./ClientExample"
		     << " <instance name> <zks> <user> <password>"
		     << " <optional hdfsNN> <optional hdfsPort>" << std::endl;
		exit (1);
	}

	std::string nameNode = "";
	uint16_t nnPort = 0;
	if (argc == 6) {
		std::cout << "Arguments must contains namenode port" << std::endl;
		exit (1);
	} else if (argc == 7) {
		nameNode = argv[5];
		nnPort = atoi (argv[6]);
	}

	std::string table = "InsertTest";

	run_test(table,argv[1],argv[2],argv[3],argv[4],-1);
}

