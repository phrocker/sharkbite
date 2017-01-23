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

#ifndef HEURISTIC_H_
#define HEURISTIC_H_

#include "../../interconnect/ClientInterface.h"

namespace scanners
{
/**
 * Heuristic function that defines how the client will connect
 **/
template<typename T>
class Heuristic
{

protected:

	std::vector<interconnect::ClientInterface<T>*> servers;

	virtual void addClientInterface(interconnect::ClientInterface<T> *ifc)
	{
		servers.push_back(ifc);
	}
	
	


public:
	/**
	 * Constructor.
	 **/
	Heuristic()
	{
	}

	/**
	 * Destructor deletes the connecting interfaces
	 **/
	virtual ~Heuristic()
	{
		/**
		 * we maintain ownership of the client interface at this point
		 **/
		for(auto ifc : servers) {
			delete ifc;
		}

	}
	
	

};
}
#endif /* HEURISTIC_H_ */
