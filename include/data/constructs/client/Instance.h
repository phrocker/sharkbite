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
#ifndef INSTANCE_H_
#define INSTANCE_H_

#include <memory>
#include <string>

#include <vector>



#include "../security/AuthInfo.h"
#include "InstanceCache.h"
#include "../configuration/Configuration.h"
#include "../include/interconnect/transport/ServerConnection.h"

#include <string>



namespace cclient {
namespace data {



/**
 * Instance represents a connecting object for the key value store
 * Nearly all key value stores follow a root/worker paradigm.
 * This class is meant to facilitate that paradigm.
 * 
 **/
class Instance {
public:
    Instance() {

    }
    /**
     * Returns the controller
     * @return root location
     **/
    virtual std::string getRootTabletLocation() = 0;
    /**
     * Returns the controller
     * @return root location
     **/
    virtual std::vector<std::string> getMasterLocations() = 0;
    /**
     * Returns a list of server that will do work
     * @return a vector of serverconnections
     **/
    virtual std::vector<std::shared_ptr<interconnect::ServerConnection>> getServers() = 0;
    /**
      * return instance ID
      * @return instance ID
      **/
    virtual std::string getInstanceId() = 0;
    /**
     * Return instance namespace
     * @return instance name
     **/
    virtual std::string getInstanceName() = 0;
    /**
      * Return link to instance instance cache
      * @return instance cache
      **/
    virtual InstanceCache *getInstanceCache() =0;
    /**
     * Returns configuration
     * @return configuration reference
     **/
    virtual const cclient::impl::Configuration *getConfiguration() = 0;

    /**
     * Destructor
     **/
    virtual ~Instance() {

    }
};

} /* namespace impl */
} /* namespace cclient */
#endif /* INSTANCE_H_ */

