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
#ifndef SRC_CLIENTEXAMPLE_H_
#define SRC_CLIENTEXAMPLE_H_



#ifdef __cplusplus
extern "C" {
#endif
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include "accumulo_data.h"
#include <data/constructs/Key.h>
struct TableOps {
	char *table_name;
	// stuff you don't use
	struct connector *parent;
	void *tableOpsPtr;
};

struct BatchScan {
	struct TableOps *tableOps;
	// stuff you don't use
	void *scannerPtr;
	void *res;
	void *on_next_fn;
};

struct connector {

	// stuff you don't use
	void *masterPtr;
	void *zk;

};

struct connector *create_connector(char *instance, char *zks, char *username,
		char *password);

int free_connector(struct connector *connector);

struct TableOps *open_table(struct connector *connector, char *tableName);

struct TableOps *create_table(struct connector *connector, char *tableName);

int free_table(struct TableOps *tableOps);

// scanner code

struct BatchScan *createScanner(struct TableOps *tableOps, short threads);

int addRange(struct BatchScan *scanner, struct CRange *range);

int addRanges(struct BatchScan *scanner, struct CRange **range, int size);

bool hasNext(struct BatchScan *scanner);

int onNext(struct BatchScan *scanner, struct CKeyValue *kv);

int next(struct BatchScan *scanner, struct CKeyValue *kv);

int nextMany(struct BatchScan *scanner, struct KeyValueList *kvl);

int closeScanner(struct BatchScan *scanner);




// writer code

std::shared_ptr<cclient::data::Key> toKey(CKey *key);

void populateKey(CKey *key, const std::shared_ptr<cclient::data::Key> &otherKey);


#ifdef __cplusplus
}

//end extern "C"
#endif

#endif
