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
#include <fstream>
#include <string>
#include <set>
#include <netinet/in.h>
#include <stdint.h>
#include "../../include/data/constructs/compressor/compressor.h"
#include "../../include/data/constructs/compressor/zlibCompressor.h"
#include "../../include/data/constructs/rfile/RFile.h"

#include "../../include/data/constructs/Key.h"
#include "../../include/data/constructs/value.h"
#include "../../include/data/constructs/KeyValue.h"
#include "../../include/data/constructs/rkey.h"
#include <sys/time.h>


#define CATCH_CONFIG_MAIN

#define NUMBER 50000
#include "../catch.hpp"


/**
 * This test simply verifies that an RFile can be created.
 * It must be verified externally
 */


std::ifstream::pos_type filesize(const char* filename)
{
    std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
    return in.tellg(); 
}

TEST_CASE("Create rfile", "[CreateRfile]") {
	for (int j = 0; j < 1; j++) {


		std::ofstream ofs ("/tmp/test.rf", std::ofstream::out);

		cclient::data::streams::OutputStream *stream = new cclient::data::streams::OutputStream(&ofs,0);


//		fstream fileStream("/test.rf",
	//			fstream::in | fstream::out | fstream::trunc);

		cclient::data::compression::Compressor *compressor = new cclient::data::compression::ZLibCompressor(256*1024);

		cclient::data::BlockCompressedFile bcFile(compressor);

		cclient::data::streams::ByteOutputStream *outStream = new cclient::data::streams::BigEndianByteStream(250 * 1024 * 1024,stream);
		cclient::data::RFile *newRFile = new cclient::data::RFile(outStream, &bcFile);

		std::set<std::shared_ptr<cclient::data::KeyValue> > keyValues;

		std::shared_ptr<cclient::data::Key> prevKey = NULL;
		struct timeval start, end;
		long mtime, seconds, useconds;

		char rw[13], cf[9], cq[9], cv[9];
		int i = 0;
		for (i = 0; i < NUMBER; i++) {

			std::shared_ptr<cclient::data::Key> k = std::make_shared<cclient::data::Key>();
			std::shared_ptr<cclient::data::Value> v = std::make_shared<cclient::data::Value>();
			std::string rowSt = "2";

			memset(rw, 0x00, 13);
			sprintf(rw, "%08d", i);

			k->setRow((const char*) rw, 8);

			sprintf(cf, "%03d", i);

			k->setColFamily((const char*)  cf, 3);

			sprintf(cq, "%08d", i);
			sprintf(cv, "%08d", i);

			k->setColQualifier((const char*)  cq, 8);
			k->setColVisibility((const char*)  cv, 8);

			std::shared_ptr<cclient::data::KeyValue> kv = std::make_shared<cclient::data::KeyValue>();


			kv->setKey(k,true);
			kv->setValue(v);

			keyValues.insert(kv);
			prevKey = k;
		}
		std::cout << "Time to actually insert " << i << std::endl;
		gettimeofday(&start, NULL);
		newRFile->addLocalityGroup();
		for(std::set<std::shared_ptr<cclient::data::KeyValue> >::iterator it = keyValues.begin(); it != keyValues.end(); ++it)
		{
			newRFile->append(*it);
		}


		newRFile->close();
		
		
		

		gettimeofday(&end, NULL);

		delete outStream;

		seconds = end.tv_sec - start.tv_sec;
		useconds = end.tv_usec - start.tv_usec;

		mtime = ((seconds) * 1000 + useconds / 1000.0) + 0.5;
		std::cout << "Elapsed Time: " << mtime << " milliseconds" << std::endl;
		delete newRFile;
		delete stream;


	}

}
