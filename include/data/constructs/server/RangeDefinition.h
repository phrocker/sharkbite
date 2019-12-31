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

#ifndef RANGEDEFINITION_H_
#define RANGEDEFINITION_H_

#include <string>
#include <vector>
#include <iostream>

#include "ServerDefinition.h"
#include "../security/AuthInfo.h"
#include "../Range.h"
#include "../column.h"
#include "../KeyExtent.h"

namespace cclient {

namespace data {
namespace tserver {

/**
 * Range definition is a server definition that contains the key extent and
 * ranges in which we are accessing
 *
 * Design: extends Server definition
 */
class RangeDefinition: public ServerDefinition {
public:
	/**
	 * Constructor
	 * @param creds ptr to credentials
	 * @param auths pointer to auths used in range definition
	 * @param host host name we're connecting to
	 * @param port port connecting to
	 * @param keyRange range
	 * @param keyExt key extent
	 *
	 */
	RangeDefinition(cclient::data::security::AuthInfo *creds,
			cclient::data::security::Authorizations *auths, std::string host,
			uint32_t port, std::vector<std::shared_ptr<cclient::data::Range>> *keyRange,
			std::vector<std::shared_ptr<KeyExtent>> *keyExt,
			const std::vector<Column> &columns);

	RangeDefinition(cclient::data::security::AuthInfo *creds,
			cclient::data::security::Authorizations *auths, std::string host,
			uint32_t port, std::vector<std::shared_ptr<cclient::data::Range>> *keyRange,
			std::vector<std::shared_ptr<KeyExtent>> *keyExt);

	/**
	 * Returns ranges
	 * @returns ranges
	 */
	std::vector<std::shared_ptr<Range>>* getRanges();

	/**
	 * Returns key extents
	 * @returns key extents;
	 */
	std::vector<std::shared_ptr<KeyExtent>>* getExtents();

	/**
	 * Returns a pointer to the columns created in this server definition.
	 **/
	std::vector<Column> getColumn() const {
		return columns;
	}

	virtual ~RangeDefinition() {

	}

	friend inline std::ostream&
	operator <<(std::ostream &out, RangeDefinition &rhs) {
		out << "Ranges: ";
		for (const auto range : rhs.ranges) {
			out << *range << " ";
		}
		out << "KeyExtents: ";
		for (const auto extent : rhs.extents) {
			out << extent << " ";
		}
		return out;
	}

	bool operator==(const RangeDefinition *rhs) const;

	bool operator==(const RangeDefinition &other) const;

protected:
	std::vector<std::shared_ptr<cclient::data::Range>> ranges;
	std::vector<std::shared_ptr<KeyExtent>> extents;
	std::vector<Column> columns;
};
}
}
}
#endif /* RANGEDEFINITION_H_ */
