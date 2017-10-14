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
#ifndef BASE_TRANSPORT_H
#define BASE_TRANSPORT_H


#include <chrono>
#include <thread>
#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand
#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

#include <concurrency/ThreadManager.h>

#include <sys/time.h>


#include "Transport.h"
#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>

#include "../../data/extern/thrift/ClientService.h"
#include "../../data/extern/thrift/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "ServerTransport.h"
#include "../../data/extern/thrift/ThriftWrapper.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../Scan.h"



namespace interconnect
{

class ThriftTransporter: virtual public ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent,
	cclient::data::Range*, cclient::data::Mutation*>
{
protected:
	std::map<cclient::data::security::AuthInfo*,org::apache::accumulo::core::security::thrift::TCredentials> convertedMap;
	boost::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport;
	org::apache::accumulo::core::client::impl::thrift::ClientServiceClient *client;
	org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient *tserverClient;

	ServerConnection *clonedConnection;

	virtual void newTransporter(ServerConnection *conn)
	{

		clonedConnection = new ServerConnection(conn);

		boost::shared_ptr<apache::thrift::transport::TSocket> serverTransport(
		        new apache::thrift::transport::TSocket(conn->getHost(), conn->getPort()));
		
		//serverTransport->setLinger(false,0);
		//serverTransport->setNoDelay(false);
		//serverTransport->setConnTimeout(0);

		boost::shared_ptr<apache::thrift::transport::TTransport> transporty(
		        new apache::thrift::transport::TFramedTransport(serverTransport));

		try
		{
			std::cout << "attempting to connect to ! to " << conn->getHost() << " and " << conn->getPort() << std::endl;
		  transporty->open();

		  std::cout << "connected! to " << conn->getHost() << " and " << conn->getPort() << std::endl;
		}
		catch( apache::thrift::transport::TTransportException te)
		{
			std::cout << conn->getHost() << " host-port " << conn->getPort() << te.what() <<  " " << std::endl;
		    try
		    {
		      transporty->close();
		    }
		    catch( apache::thrift::transport::TTransportException to )
		    {
		    	std::cout << conn->getHost() << " host-port " << conn->getPort() << te.what() <<  " " << std::endl;
		    }
		    throw te;
		}

		underlyingTransport = transporty;


	}

	boost::shared_ptr<apache::thrift::transport::TTransport> createTransporter()
	{
		
		boost::shared_ptr<apache::thrift::transport::TSocket> serverTransport(
		        new apache::thrift::transport::TSocket(clonedConnection->getHost(), clonedConnection->getPort()));
		
		serverTransport->setLinger(false,0);
		serverTransport->setNoDelay(true);
		serverTransport->setConnTimeout(0);

		boost::shared_ptr<apache::thrift::transport::TTransport> transporty(
		        new apache::thrift::transport::TFramedTransport(serverTransport));

		transporty->open();

		return transporty;


	}

	Scan *
	singleScan(ScanRequest<ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> > *request)
	{
		Scan *initialScan = new Scan();

		org::apache::accumulo::core::data::thrift::InitialScan scan;

		org::apache::accumulo::core::trace::thrift::TInfo scanId;

		scanId.parentId = 0;
		scanId.traceId = rand();

		std::vector<cclient::data::IterInfo*> *iters = request->getIterators();
		std::map<std::string, std::map<std::string, std::string> > iterOptions;
		for (auto it = iters->begin(); it != iters->end(); it++) {
			auto myOptions = (*it)->getOptions();
			for (auto optIt = myOptions.begin(); optIt != myOptions.end();
			     optIt++) {
				iterOptions[(*it)->getName()][(*optIt).first] = (*optIt).second;
			}
		}


		ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> *ident =
		        request->getRangeIdentifiers()->at(0);
		cclient::data::KeyExtent *extent = ident->getGlobalMapping().at(0);
		cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
		org::apache::accumulo::core::security::thrift::TCredentials creds = getOrSetCredentials(request->getCredentials());
		tserverClient->startScan(scan, scanId,creds
		                         ,
		                         ThriftWrapper::convert(extent), ThriftWrapper::convert(range),
		                         ThriftWrapper::convert(request->getColumns()), 1024,
		                         ThriftWrapper::convert(iters), iterOptions,
		                         request->getAuthorizations()->getAuthorizations(), true, false,
		                         1024);


		org::apache::accumulo::core::data::thrift::ScanResult results =
		        scan.result;

		std::vector<cclient::data::KeyValue*> *kvs = ThriftWrapper::convert(results.results);


		initialScan->setHasMore(results.more);

		
		initialScan->setScanId(scan.scanID);

		initialScan->setNextResults(kvs);

		if (!results.more) {
			tserverClient->closeScan(scanId, scan.scanID);
		}

		delete kvs;

		return initialScan;
	}

	Scan *
	multiScan(ScanRequest<ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> > *request)
	{
		Scan *initialScan = new Scan();

		org::apache::accumulo::core::data::thrift::InitialMultiScan scan;

		org::apache::accumulo::core::trace::thrift::TInfo scanId;

		scanId.traceId = scan.scanID;
		scanId.parentId = scan.scanID;

		std::vector<cclient::data::IterInfo*> *iters = request->getIterators();
		std::map<std::string, std::map<std::string, std::string> > iterOptions;
		for (auto it = iters->begin(); it != iters->end(); it++) {
			auto myOptions = (*it)->getOptions();
			for (auto optIt = myOptions.begin(); optIt != myOptions.end();
			     optIt++) {
				iterOptions[(*it)->getName()][(*optIt).first] = (*optIt).second;
			}
		}

		tserverClient->startMultiScan(scan, scanId,
		                              ThriftWrapper::convert(request->getCredentials()),
		                              ThriftWrapper::convert(request->getRangeIdentifiers()),
		                              ThriftWrapper::convert(request->getColumns()),
		                              ThriftWrapper::convert(iters), iterOptions,
		                              request->getAuthorizations()->getAuthorizations(), true);

		org::apache::accumulo::core::data::thrift::MultiScanResult results =
		        scan.result;

		std::vector<cclient::data::KeyValue*> *kvs = ThriftWrapper::convert(results.results);

		initialScan->setHasMore(results.more);

		initialScan->setNextResults(kvs);

		tserverClient->closeMultiScan(scanId, scan.scanID);

		delete kvs;

		return initialScan;
	}
	org::apache::accumulo::core::security::thrift::TCredentials getOrSetCredentials(cclient::data::security::AuthInfo *convert)
	{
	  std::map<cclient::data::security::AuthInfo*,org::apache::accumulo::core::security::thrift::TCredentials>::iterator it;
	  it = convertedMap.find(convert);
	  if (it != convertedMap.end())
	  {
	    return it->second;
	  }
	  
	  org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(convert);
	  convertedMap.insert(std::pair<cclient::data::security::AuthInfo*,org::apache::accumulo::core::security::thrift::TCredentials>(convert,creds));
	  return creds;
	
	    
	}

public:

	explicit ThriftTransporter(ServerConnection *conn) :
		interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*,
		cclient::data::Mutation*>(conn), client(NULL), tserverClient(NULL)
	{

		newTransporter(conn);
	}

	virtual ~ThriftTransporter()
	{
		underlyingTransport->close();
		delete clonedConnection;
		if (NULL != tserverClient)
			delete tserverClient;
		if (NULL != client)
			delete client;
	}

	apache::thrift::transport::TTransport getTransport()
	{
		return *underlyingTransport;
	}

	void authenticate(cclient::data::security::AuthInfo *auth)
	{

		org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();

		try{
		  if (!client->authenticateUser(tinfo, creds, creds)) {
			  throw cclient::exceptions::ClientException("Invalid username");
		  }
		}catch(org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException tse)
		{
		  throw cclient::exceptions::ClientException(INVALID_USERNAME_PASSWORD);
		}
	}
	
	void createIfClosed()
	{
	     if (underlyingTransport.get() != NULL && !underlyingTransport->isOpen())
	    {
	 
	      underlyingTransport = createTransporter();
	      createClientService();
	    }
	}
	
	
	void closeAndCreateClient()
	{
	     if (underlyingTransport.get() != NULL &&  underlyingTransport->isOpen())
	    {
	      underlyingTransport->close();
	    }

	    underlyingTransport = createTransporter();
	    createClientService();
	}
	
	void createClientService()
	{
	 
	    boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(
		        new apache::thrift::protocol::TCompactProtocol(underlyingTransport));

		  if (NULL != client)
		{
		  delete client;
		  client = NULL;
		}
		if (NULL != tserverClient)
		{
		  delete tserverClient;
		  tserverClient = NULL;
		}
		client =
		        new org::apache::accumulo::core::client::impl::thrift::ClientServiceClient(
		        protocolPtr);
		tserverClient =
		        new org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient(
		        protocolPtr);
	}
	

	virtual void registerService(std::string instance,
	                             std::string clusterManagers)
	{
		boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(
		        new apache::thrift::protocol::TCompactProtocol(underlyingTransport));
			
		
		if (NULL != client)
		{
		  delete client;
		  client = NULL;
		}
		if (NULL != tserverClient)
		{
		  delete tserverClient;
		  tserverClient = NULL;
		}

		client =
		        new org::apache::accumulo::core::client::impl::thrift::ClientServiceClient(
		        protocolPtr);
		tserverClient =
		        new org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient(
		        protocolPtr);


		client->getZooKeepers(clusterManagers);
		client->getInstanceId(instance);
	}

	Scan *
	beginScan(ScanRequest<ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> > *request)
	{
		Scan *initialScan = NULL;
		if (request->getRangeIdentifiers()->size() > 1) {
			initialScan = multiScan(request);
		} else {
		  ScanIdentifier<cclient::data::KeyExtent*, cclient::data::Range*> *ident =
		        request->getRangeIdentifiers()->at(0);
			cclient::data::KeyExtent *extent = ident->getGlobalMapping().at(0);
			cclient::data::Range *range = ident->getIdentifiers(extent).at(0);
			if (range->getStartKey() == NULL && range->getStopKey() == NULL)
			{
			  initialScan = multiScan(request);
			}
			else
			  initialScan = singleScan(request);

		}
		return initialScan;
	}


	Scan *
	continueScan(Scan *originalScan)
	{
		org::apache::accumulo::core::data::thrift::ScanResult results;
		org::apache::accumulo::core::trace::thrift::TInfo tinfo;

		org::apache::accumulo::core::data::thrift::ScanID scanId = originalScan->getId();


		tinfo.traceId = originalScan->getId()+1;
		tinfo.parentId = originalScan->getId();
		tserverClient->continueScan(results,tinfo,scanId);

		std::vector<cclient::data::KeyValue*> *kvs = ThriftWrapper::convert(results.results);


		if (results.more)
		  originalScan->setTopKey(kvs->back()->getKey());
		
		originalScan->setHasMore(results.more);

		originalScan->setNextResults(kvs);

		if (!results.more) {
			tinfo.traceId++;
			tserverClient->closeScan(tinfo, originalScan->getId());
		}

		delete kvs;


		return originalScan;
	}


	void *write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<cclient::data::Mutation*>> *request)
	{

		org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		org::apache::accumulo::core::data::thrift::UpdateID upId =
		        tserverClient->startUpdate(tinfo, creds,org::apache::accumulo::core::tabletserver::thrift::TDurability::DEFAULT);
		for (std::map<cclient::data::KeyExtent, std::vector<cclient::data::Mutation*>>::iterator it = request->begin();
		     it != request->end(); it++) {

			tserverClient->applyUpdates(tinfo, upId,
			                            ThriftWrapper::convert(it->first),
			                            ThriftWrapper::convert(&it->second));
		}
		tinfo.parentId=tinfo.traceId;
		tinfo.traceId=tinfo.traceId+1;
		org::apache::accumulo::core::data::thrift::UpdateErrors errors;
		tserverClient->closeUpdate(errors, tinfo, upId);
		//@TODO return errors
		return 0;
	}

	bool dropUser(cclient::data::security::AuthInfo *auth, std::string user )
	{
	     org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		try
		{
		  client->dropLocalUser(tinfo,creds,user);
		  return true;
		}
		catch(org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException tse)
		{
		  // could not create the user for some reason
		  return false;
		}
	}
	
	bool changeUserPassword(cclient::data::security::AuthInfo *auth, std::string user, std::string password )
	{
	     org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		
		try
		{
		  client->changeLocalUserPassword(tinfo,creds,user,password);
		  return true;
		}
		catch(org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException tse)
		{
		  // could not create the user for some reason
		  return false;
		  
		}
	}
	bool createUser(cclient::data::security::AuthInfo *auth, std::string user, std::string password)
	{
	    org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		try
		{
		  client->createLocalUser(tinfo,creds,user,password);
		  return true;
		}
		catch(org::apache::accumulo::core::client::impl::thrift::ThriftSecurityException tse)
		{
		  // could not create the user for some reason
		  return false;
		  
		}
		
	}
	
	std::map<std::string,std::string> getTableConfiguration(cclient::data::security::AuthInfo *auth, std::string table)
	{
	  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		std::map<std::string,std::string> ret;
		client->getTableConfiguration(ret,tinfo,creds,table);
	  
	  return ret;
	}
	
	
	cclient::data::security::Authorizations *getUserAuths(cclient::data::security::AuthInfo *auth, std::string user)
	{
	   org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		std::vector<std::string> returnStrings;
		client->getUserAuthorizations(returnStrings,tinfo,creds,user);
		return new cclient::data::security::Authorizations(&returnStrings);
	}
	
	
	 void changeUserAuths(cclient::data::security::AuthInfo *auth, std::string user, cclient::data::security::Authorizations *auths)
	{
	  org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		tinfo.parentId = 0;
		tinfo.traceId = rand();
		  client->changeAuthorizations(tinfo,creds,user,auths->getAuthorizations());
	}
	
	
	void splitTablet(cclient::data::security::AuthInfo *auth, cclient::data::KeyExtent *extent, std::string split)
	{
	   org::apache::accumulo::core::trace::thrift::TInfo tinfo;
		org::apache::accumulo::core::security::thrift::TCredentials creds =
		        ThriftWrapper::convert(auth);

		org::apache::accumulo::core::data::thrift::TKeyExtent ke = ThriftWrapper::convert(extent);
		tinfo.parentId = 0;
		tinfo.traceId = rand();
		tserverClient->splitTablet(tinfo,creds,ke,split);
	}
	void close()
	{
	  if (NULL != client)
	  {
	    delete client;
	    client = NULL;
	  }
	  if (NULL != tserverClient)
	  {
	    delete tserverClient;
	    tserverClient = NULL;
	  }
	  underlyingTransport->close();
	}

	void close(Scan *scan)
	{
	  
	}

	bool open()
	{
	  return underlyingTransport->isOpen();
	}

	bool isOpen()
	{
	  return underlyingTransport->isOpen();
	}

};
}
#endif
