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
 * See the License for the specific language governing premissions and
 * limitations under the License.
 */

#include <iostream>
#include <sstream>
#include <fstream>

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


#include <assert.h>

#define BOOST_IOSTREAMS_NO_LIB 1

bool
keyCompare (std::shared_ptr<cclient::data::KeyValue>  a, std::shared_ptr<cclient::data::KeyValue>  b)
{
	return *(a->getKey ()) < *(b->getKey ());
}

std::pair<std::string, std::string>
writeRfile (std::string nameNode, uint16_t port)
{


	std::string dir = "/testImport/";
	std::string fail = "/testImportFail/";
	std::string path = dir;
	path.append ("test.rf");


  std::ofstream ofs ("/tmp/test.rf", std::ofstream::out);

  cclient::data::streams::OutputStream *stream = new cclient::data::streams::OutputStream(&ofs,0);

	cclient::data::compression::Compressor *compressor = new cclient::data::compression::ZLibCompressor (256 * 1024);

	cclient::data::BlockCompressedFile bcFile (compressor);

	cclient::data::streams::EndianTranslationStream *outStream = new cclient::data::streams::EndianTranslationStream (stream);

	cclient::data::RFile *newRFile = new cclient::data::RFile (outStream, &bcFile);

	std::vector<std::shared_ptr<cclient::data::KeyValue> > keyValues;

	std::shared_ptr<cclient::data::Key> prevKey = NULL;

	char rw[13], cf[4], cq[8], cv[8];
	int i = 0;

	std::string moto = "hello moto";
	std::string vis = "00000001";
	for (i = 1; i < 150; i++) {

		std::shared_ptr<cclient::data::Value> v = std::make_shared< cclient::data::Value >(moto);

		std::shared_ptr<cclient::data::Key> k = std::make_shared< cclient::data::Key> ();

		std::string rowSt = "2";

		memset (rw, 0x00, 13);
		sprintf (rw, "bat");

		k->setRow ((const char*) rw, 8);

		sprintf (cf, "%03d", i);

		k->setColFamily ((const char*) cf, 3);

		sprintf (cq, "%08d", i);
		sprintf (cv, "%08d", i);

		k->setColQualifier ((const char*) cq, 8);
		k->setColVisibility (vis.c_str (), vis.size ());

		k->setTimeStamp (1445105294261L);

		std::shared_ptr<cclient::data::KeyValue> kv = std::make_shared< cclient::data::KeyValue> ();

		kv->setKey (k);
		kv->setValue (v);

		keyValues.push_back (kv);
		prevKey = k;
	}
	std::sort (keyValues.begin (), keyValues.end (), keyCompare);
	newRFile->addLocalityGroup ();
	for (std::vector<std::shared_ptr<cclient::data::KeyValue> >::iterator it = keyValues.begin ();
	     it != keyValues.end (); ++it) {
		newRFile->append (*it);
	}

	newRFile->close ();



	outStream->flush ();
	stream->flush ();

	delete outStream;
	delete stream;

	delete newRFile;


	dir = "/testImport/";
	fail = "/testImportFail/";
	std::stringstream nd;
	nd << nameNode << ":" << port << dir;
	path = nd.str();
	std::stringstream faildir;
	faildir << nameNode << ":" << port << fail;
	fail = faildir.str();
	return std::make_pair (path, fail);
}

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

	std::string table = "blah2";

	
	
	cclient::data::zookeeper::ZookeeperInstance *instance = 0;
	
	try{
	cclient::impl::Configuration *confC = (new cclient::impl::Configuration());
	confC->set ("FILE_SYSTEM_ROOT", "/accumulo");
	
	instance = new cclient::data::zookeeper::ZookeeperInstance (argv[1],argv[2], 1000,
	                std::unique_ptr<cclient::impl::Configuration>(confC));
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Could not connect to ZK. Error: " << ce.what()  << std::endl;
	  return 1;
	}

	cclient::data::security::AuthInfo creds (argv[3], argv[4], instance->getInstanceId ());

	interconnect::MasterConnect *master = 0;
	
	try{
	master = new interconnect::MasterConnect (&creds, instance);
	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Could not connect: " << ce.what() << std::endl;
	  return 1;
	}
	std::unique_ptr<interconnect::AccumuloTableOperations> ops = master->tableOps (
	                        table);

	
	std::unique_ptr<interconnect::SecurityOperations> secOps = master->securityOps();

	// create the table. no harm/no foul if it exists
	std::cout << "Checking if " << table << " exists." << std::endl;
	if (!ops->exists ()) {
		std::cout << "Now, creating " << table << std::endl;
		if (!ops->create ()) {
			std::cout << "Could not create table " << std::endl;
		}
		std::this_thread::sleep_for (std::chrono::milliseconds (1000));
	}



	std::unique_ptr<interconnect::NamespaceOperations> nameOps = master->namespaceOps();


	cclient::data::security::Authorizations auths;
	
	cclient::data::security::Authorizations scanAuths;
	scanAuths.addAuthorization("00000001");
	
	if (secOps->createUser("dude","thedude"))
	{
	
	  // now let's drop the user
	
	  secOps->dropUser("dude");
	  
	}
	else
	  std::cout << "Could not create user 'dude'" << std::endl;
	
	secOps->grantAuthorizations(&scanAuths,"root");

	int fruit_to_write = 200;

	std::cout << "Writing " << fruit_to_write << " apples and bananas" << std::endl;

	std::unique_ptr<writer::BatchWriter> writer = ops->createWriter (&auths, 15);

	for (int i = 0; i < fruit_to_write; i++) {

		std::shared_ptr<cclient::data::KeyValue> newKv = std::make_shared<cclient::data::KeyValue> ();
		std::shared_ptr<cclient::data::Key> newKey = std::make_shared<cclient::data::Key> ();
		newKey->setRow ("a", 1);
		newKey->setColFamily ("apple", 5);
		std::stringstream cq;
		cq << "banana" << i;
		newKey->setColQualifier (cq.str ().c_str (), cq.str ().length ());
		newKey->setTimeStamp(1445105294261L);
		newKv->setKey (newKey);
		newKv->setValue (std::make_shared<cclient::data::Value> ());
		writer->push (newKv);
	}
	
	for (int i = 0; i < fruit_to_write; i++) {

		cclient::data::Mutation *newKv = new cclient::data::Mutation ("a");
		
		std::stringstream cq;
		cq << "banana" << i;
		
		newKv->put("avacado",cq.str(),"",1445105294261L);
		
		writer->addMutation (std::unique_ptr<cclient::data::Mutation>(newKv));
	}
	
	

	// close will free memory for objects given to it
	writer->close ();

	if ( ops->flush("a","z",true) ) {
		std::cout << "flush successful " << std::endl;
	}


	if (ops->compact("a","z",true)) {
		std::cout << "Compaction successful " << std::endl;
	}
	

      //scan with 10 threads
      int counter = 0;
      try{
	std::cout << "Reading values from row a to d" << std::endl;

	std::unique_ptr<scanners::BatchScanner> scanner = ops->createScanner (&auths, 1);

	// range from a to d
	std::shared_ptr<cclient::data::Key> startkey = std::make_shared<cclient::data::Key>();
	startkey->setRow ("a", 1);
	std::shared_ptr<cclient::data::Key> stopKey= std::make_shared<cclient::data::Key>();
	stopKey->setRow ("z", 1);
	cclient::data::Range *range = new cclient::data::Range (startkey, true, stopKey, false);

	scanner->addRange (std::unique_ptr<cclient::data::Range>(range));
	
	scanner->fetchColumn("avacado");

	scanners::Iterator<cclient::data::KeyValue> *results =
	                scanner->getResultSet ();

	
	
	for (auto iter = results->begin (); iter != results->end ();
	     iter++, counter++) {
		auto kv = *iter;

		if (kv != NULL && kv->getKey () != NULL)
			std::cout << "got -- " << (*iter)->getKey () << std::endl;
		else
			std::cout << "Key is null" << std::endl;
	}

	}catch(cclient::exceptions::ClientException ce)
	{
	  std::cout << "Failed message: " << ce.what() << std::endl;
	}

	std::cout << "Received " << counter << " results " << std::endl;
	if (counter >= 200)
	{
	  std::cout << "Removing table" << std::endl;
	
	  ops->remove ();
	}
	else
	{
	  std::cout << "did not get expected results. Please look into this" << std::endl;
	}

	//assert(counter == fruit_to_write/2 );

	delete master;

	delete instance;

	return 0;
}

