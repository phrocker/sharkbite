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
#include <fstream>
#include <iostream>
#include <sstream>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/data/constructs/client/zookeeperinstance.h"
#include "../include/interconnect/Master.h"
#include "../include/interconnect/tableOps/TableOperations.h"

#include "../include/data/constructs/rfile/RFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"

#include "../include/data/streaming/HdfsStream.h"
#include "../include/data/streaming/OutputStream.h"

#define BOOST_IOSTREAMS_NO_LIB 1



std::ifstream::pos_type filesize(const char* filename)
{
    std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
    return in.tellg();
}


void
readRfile (std::string outputFile, uint16_t port, bool dump)
{
    struct hdfsBuilder *builder = hdfsNewBuilder ();

    std::ifstream ifs (outputFile.c_str(), std::ifstream::binary | std::ifstream::in);


    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(&ifs,0);


    std::fstream::pos_type size = filesize(outputFile.c_str());
    std::cout << "tellg is " << size << std::endl;
    cclient::data::RFile *newRFile = new cclient::data::RFile (stream, size);

    std::cout << newRFile << std::endl;

    if (dump)
    {

        std::vector<uint8_t*> cf;
        cclient::data::streams::StreamSeekable *seekable = new cclient::data::streams::StreamSeekable(new cclient::data::Range(),cf,false);


        newRFile->relocate(seekable);
        while (newRFile->hasNext())
        {
            std::cout << "has next " << (**newRFile).first <<  std::endl;

            newRFile->next();

            if (!newRFile->hasNext())
            {
                std::cout << "no next" << std::endl;
            }

        }

    }

    std::cout << "we done" << std::endl;

    delete stream;

    delete newRFile;

    hdfsFreeBuilder(builder);

}
int
main (int argc, char **argv)
{

    if (argc < 2)
    {
        std::cout << "Arguments required: ./PrintInfo"
             << " <output file>" << std::endl;
        exit (1);
    }

    std::string inputFile = argv[1];

    bool dump = false;
    if (argc == 3)
    {
        if (!strcmp(argv[2],"-d") || !strcmp(argv[2],"dump"))
        {
            dump = true;
        }
    }

    if (!IsEmpty (&inputFile))
    {
        readRfile(inputFile, 0 , dump);
    }



    return 0;
}

