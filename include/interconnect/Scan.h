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

#ifndef SCAN_H_
#define SCAN_H_ 1


#include <vector>
#include <stdio.h>      /* printf, scanf, puts, NULL */
#include <stdlib.h>     /* srand, rand */
#include <time.h>

#include "../data/constructs/KeyValue.h"

//http://sector.sourceforge.net/software.html

/**
 * Represents a running scan
 */

namespace interconnect
{

/**
 * Scan representation
 * 
 **/
class Scan
{
public:

	Scan();

	~Scan();

	/**
	 * Sets the next result set.
	 * @param resultSet result set to add to this object's results
	 * @returns result of adding. 
	 **/
	bool setNextResults(std::vector<cclient::data::KeyValue*> *resultSet)
	{
		results.insert(results.end(), resultSet->begin(),
		               resultSet->end());
		return true;
	}

	/**
	 * Gets the next result and places it into resultSet
	 * @param reference result set to add. 
	 **/
	bool getNextResults(std::vector<cclient::data::KeyValue*> *resultSet)
	{
		resultSet->insert(resultSet->end(), results.begin(), results.end());
		results.clear();
		return hasMore;
	}

	/**
	 * Set flag to identify that more results are available.
	 * @param more more results
	 **/
	void setHasMore(bool more)
	{
		hasMore = more;
	}

	/**
	 * Gets variable to identify if more results are available
	 * @returns whether or not there are more results.
	 **/
	bool getHasMore()
	{
		return hasMore;
	
	}
	/**
	 * Returns the scan id
	 * @returns scan id.
	 **/
	int64_t getId()
	{

		return scanId;

	}

	/**
	 * Sets the scan id
	 * @param scanId scan identifier
	 **/
	void setScanId(int64_t scanId)
	{
		this->scanId = scanId;
	}
	
	void setTopKey(cclient::data::Key *key)
	{
	  topKey = key;
	}
	
	cclient::data::Key *getTopKey() const
	{
	  return topKey;
	}

protected:
	cclient::data::Key *topKey;
	// scan id
	int64_t scanId;
	// has more results.
	bool hasMore;
	// results
	std::vector<cclient::data::KeyValue*> results;
};
}
#endif /* SCAN_H_ */
