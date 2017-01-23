CPPFLAGS=-O3 --std=c++11 -lthrift -I/usr/local/include/thrift/
LDFLAGS=
LDLIBS=
RM=rm -f

SRCS=BlockRegion.cpp data_index.cpp meta_index.cpp block_comp_stream.cpp BlockCompressedFile.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: bcfile

clean: 
	$(RM) $(OBJS)


bcfile:	$(OBJS)
	g++ -o bcfile.so $(OBJS) -shared -fPIC
	$(RM) $(OBJS)

meta_index.o: meta_index.cpp
	g++ $(CPPFLAGS) $(LDFLAGS) -c meta_index.cpp -fPIC

data_index.o: data_index.cpp
	g++ $(CPPFLAGS)  $(LDFLAGS) -c data_index.cpp -fPIC

BlockRegion.o: BlockRegion.cpp
	g++ $(CPPFLAGS)  $(LDFLAGS) -c BlockRegion.cpp -fPIC
	
BlockCompressedFile.o: BlockCompressedFile.cpp
	g++ $(CPPFLAGS)  $(LDFLAGS) -c BlockCompressedFile.cpp -fPIC

block_comp_stream.o: block_comp_stream.cpp
	g++ $(CPPFLAGS)  $(LDFLAGS) -c block_comp_stream.cpp -fPIC