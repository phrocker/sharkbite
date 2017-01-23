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

#ifndef ZOOKEEPERINSTANCE_H
#define ZOOKEEPERINSTANCE_H

#include <memory>
#include "Instance.h"
#include <sstream>

#include <string>
#include "./zookeeper/zookeepers.h"
#include "./zookeeper/zoocache.h"
#include "../inputvalidation.h"
#include "../../exceptions/ClientException.h"
#include "../configuration/Configuration.h"
#include "../../../interconnect/Master.h"


namespace cclient
{
namespace data
{
namespace zookeeper
{


/**
 * Instance that uses zookeeper
 * 
 * Purpose: provides connecting capabilities through zookeeper
 * 
 * Design: Simply inherits Instance, which is a purely virtual class.
 **/
class ZookeeperInstance: public Instance
{
public:
  
	/**
	  * ZK constructor
	  * @param in instance name
	  * @param zks zookeepers
	  * @param zkTimeoutMs timeout for zookeeper
	  * @param conf configuration object
	  **/
	ZookeeperInstance(std::string in, std::string zks, uint32_t zkTimeoutMs,
	                  std::unique_ptr<cclient::impl::Configuration> conf) :
		instanceName(in), zookeeperList(zks), timeoutMs(zkTimeoutMs), myConfiguration(
		        std::move(conf))
	{
		if (IsEmpty(&in) || IsEmpty(&zks)) {
			throw cclient::exceptions::ClientException("instance name or zookeeper list is empty");
		}

		myKeeper = new ZooKeeper(zks.c_str(), zkTimeoutMs);

		myKeeper->init(&myWatch);

		myZooCache = new ZooCache(myKeeper);

		getInstanceId();

	}
	
	/**
	  * ZK constructor
	  * @param in instance name
	  * @param zks zookeepers
	  * @param zkTimeoutMs timeout for zookeeper
	  **/
	ZookeeperInstance(std::string in, std::string zks, uint32_t zkTimeoutMs) :
		instanceName(in), zookeeperList(zks), timeoutMs(zkTimeoutMs), myConfiguration(
		        new cclient::impl::Configuration())
	{
		myConfiguration->set ("FILE_SYSTEM_ROOT", "/accumulo");
		
		if (IsEmpty(&in) || IsEmpty(&zks)) {
			throw cclient::exceptions::ClientException("instance name or zookeeper list is empty");
		}

		myKeeper = new ZooKeeper(zks.c_str(), zkTimeoutMs);

		myKeeper->init(&myWatch);

		myZooCache = new ZooCache(myKeeper);

		getInstanceId();

	}

	/**
	 * ZK Destructor
	 **/
	~ZookeeperInstance()
	{
		if (NULL != myZooCache)
		  delete myZooCache;
		if (NULL != myKeeper)
		  delete myKeeper;

	}

	/**
	  * Returns the controller
	  * @return root location
	  **/
	std::string getRootTabletLocation();

	/**
	  * Returns the controller
	  * @return root location
	  **/
	std::vector<std::string> getMasterLocations();

	/**
	  * return instance ID
	  * @return instance ID
	  **/
	std::string getInstanceId();
	/**
	  * Return instance namespace
	  * @return instance name
	  **/
	std::string getInstanceName();
	/**
	  * Returns configuration
	  * @return configuration reference
	  **/
	const cclient::impl::Configuration *getConfiguration();
	/**
	  * Sets configuration object
	  * @param configuration object
	  **/
	void setConfiguration( std::unique_ptr<cclient::impl::Configuration> conf);

	/**
	  * Returns a reference to zoocache
	  * @return zoo cache ptr
	  **/
	ZooCache *getZooCache()
	{
		return myZooCache;
	}

	/**
	  * Return link to instance instance cache
	  * @return instance cache
	  **/
	InstanceCache *getInstanceCache()
	{
		return myZooCache;
	}
	
	/**
	  * Returns a list of server that will do work
	  * @return a vector of serverconnections
	  **/
	virtual std::vector<interconnect::ServerConnection> getServers();
	
	/**
	 * Gets the root node
	 * @return root node.
	 **/
	std::string getRoot();

protected:
	// zoo cache ptr
	ZooCache *myZooCache;

	// zookeeper reference.
	ZooKeeper *myKeeper;

	// watch for the current zk
	Watch myWatch;

	// configuration object
	std::unique_ptr<cclient::impl::Configuration> myConfiguration;
	
	// instance id
	std::string instanceId;
	// instance name
	std::string instanceName;
	// zk list
	std::string zookeeperList;

	// time out in ms
	uint32_t timeoutMs;

};



}
}
}

#endif // ZOOKEEPERINSTANCE_H
