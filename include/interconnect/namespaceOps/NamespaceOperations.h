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

#ifndef NAMEOPERATIONS_H
#define NAMEOPERATIONS_H


#include "../../data/constructs/security/Authorizations.h"


#include "../../data/constructs/KeyValue.h"
#include "../../data/constructs/IterInfo.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../../data/constructs/client/Instance.h"
#include "../../scanner/Source.h"
#include "../../scanner/constructs/Results.h"
#include "../transport/AccumuloMasterTransporter.h"
#include "../RootInterface.h"



#include <map>

#include <string>
#include <mutex>




namespace interconnect
{
class NamespaceOperations
{
public:
	/**
	 * Namespaces constructor. Does not focus the namespace operations on a given instance.
	 * @param creds credentials
	 * @param instance instance provided by connector
	 * @param interface connector interface
	 * @param distributedConnector distributed interface for tablet servers
	 **/
	NamespaceOperations(cclient::data::security::AuthInfo *creds, cclient::data::Instance *instance,
	               RootInterface<interconnect::AccumuloMasterTransporter, cclient::data::KeyValue,
	               scanners::ResultBlock<cclient::data::KeyValue>> *interface, TransportPool<interconnect::AccumuloMasterTransporter> *distributedConnector) : 
	               NamespaceOperations(creds,"",instance,interface,distributedConnector)
	{
	  // simply calls the other constructor
	}
	
	/**
	 * Namespaces constructor. Does not focus the namespace operations on a given instance.
	 * @param creds credentials
	 * @param myNamespace namespace 
	 * @param instance instance provided by connector
	 * @param interface connector interface
	 * @param distributedConnector distributed interface for tablet servers
	 **/
	NamespaceOperations(cclient::data::security::AuthInfo *creds, std::string myNamespace, cclient::data::Instance *instance,
	               RootInterface<interconnect::AccumuloMasterTransporter, cclient::data::KeyValue,
	               scanners::ResultBlock<cclient::data::KeyValue>> *interface, TransportPool<interconnect::AccumuloMasterTransporter> *distributedConnector) :
	               myNamespace(myNamespace), credentials(creds), myInstance(instance),clientInterface(interface)
	{
	  refTransportPool = distributedConnector;
	  
	  loadNamespaces();
	}
	
	/**
	 * Provides a list of namespaces
	 * @return namespaces
	 **/
	std::vector<std::string> list();
	
	 /**
	  * Namespaces destructor
	  **/
	~NamespaceOperations();
	
	/**
	 * Returns a list of system namespaces
	 * @return system namespaces
	 **/
	std::string systemNamespace();
	
	
	/**
	 * Returns the default namesapce
	 * @return returns ""
	 **/
	std::string defaultNamespace(){ return ""; }
	
	/**
	 * Removes a namespace if specified, otherwise myNamespace is removed
	 * @param name namespace to remove (optional )
	 * @return status of operation.
	 **/
	bool remove(std::string name = "");
	
	/**
	 * Determines if namespace exists
	 * @param name namespace to test (optional )
	 * @return true if namespace exists, false otherwise
	 **/
	bool exists(std::string name = "");
	
	/**
	 * Creates a namespace
	 * @param name namespace to create (optional, otherwise myNamespace )
	 **/
	void create(std::string name = "");
	
	/**
	 * Renames a namespace
	 * @param newName new namespace name
	 * @param oldName previous name. if not specified myNamespace will be renamed
	 **/
	void rename(std::string newName, std::string oldName = "");
	
	/**
	 * Sets a namespace property
	 * @param property property names
	 * @param value property value
	 */
	virtual void setProperty(std::string property, std::string value, std::string nm = "");
    
	/**
	 * Removes a property on this namespace.
	 * @param property property name to remove
	 */
	virtual void removeProperty(std::string property , std::string nm = "");
	
	/**
	 * Returns all namespace properties.
	 * @return namespace properties
	 */
	virtual std::map<std::string,std::string> getProperties(std::string nm = "");
	
	/**
	 * attaches an iterator to the namesapce scope
	 * @param setting iterator settings
	 * @param scope scope on which this will run
	 * @param nm namespace. if not specified the object default is used
	 **/
	virtual void attachIterator(cclient::data::IterInfo setting, cclient::data::ITERATOR_TYPES scope, std::string nm = "");
	
	
	/**
	 * removes the iterator from the given scope
	 * @param name iterator name
	 * @param scope scope on which this will run
	 * @param nm namespace. if not specified the object default is used
	 **/
	virtual void removeIterator(std::string name, cclient::data::ITERATOR_TYPES scope, std::string nm = "");
	
protected:
  
    /**
     * Loads the namespaces
     * @param force forces the load if necessary
     **/
    void loadNamespaces(bool force = false);
    
    // client interface
    RootInterface<interconnect::AccumuloMasterTransporter, cclient::data::KeyValue,
	               scanners::ResultBlock<cclient::data::KeyValue>> *clientInterface;
    // distributed connector to tservers
    TransportPool<interconnect::AccumuloMasterTransporter> *refTransportPool;
    // instance ptr
    cclient::data::Instance *myInstance;
    // credentials
    cclient::data::security::AuthInfo *credentials;
    // provided namespace
    std::string myNamespace;
    // namespace mutex
    std::recursive_mutex namesOpMutex;
    
    // loaded namespaces
    std::map<std::string,std::string> namespaces;
    std::vector<std::string> namespaceNames;
  
};
}
#endif // USEROPERATIONS_H
