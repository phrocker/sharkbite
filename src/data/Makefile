include Makefile.inc


CPPFLAGS=-O3 --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-O3
# security/constructs.so server/constructs.so
LDLIBS= -shared -fPIC
RM=rm -f
PWD=`pwd`


DIRS	= client constructs extern/thrift streaming
OBJS=$(subst .cpp,.o,$(wildcard *.cpp))
OBJLIBS	= data-client.so constructs.so thrift.so streaming.so
LIBS	= client/data-client.so constructs/constructs.so extern/thrift/thrift.so streaming/streaming.so

#SRCS=column.cpp Key.cpp KeyExtent.cpp KeyValue.cpp Mutation.cpp Range.cpp rkey.cpp scanstate.cpp value.cpp security/$(wildcard *.cpp)



all: data

	
data:	$(OBJS) $(OBJLIBS)
#	cd security; make clean ; make ; cd $(PWD)
#	cd server; make clean ; make ; cd $(PWD)
	$(ECHO) $(LD) -o data.so $(OBJS) $(LIBS) -shared -fPIC
	g++ -o data.so $(OBJS) $(LDFLAGS) -shared -fPIC
#	$(LD) -o $(EXE) $(OBJS) $(LIBS)
	$(RM) $(OBJS)
		


constructs.so : force_look
	$(ECHO) building constructs : $(MAKE) $(MFLAGS)
	cd constructs; $(MAKE) $(MFLAGS)
	
data-client.so : force_look
	$(ECHO) building data-client : $(MAKE) $(MFLAGS)
	cd client; $(MAKE) $(MFLAGS)
	
thrift.so : force_look
	$(ECHO) building thrift : $(MAKE) $(MFLAGS)
	cd extern/thrift; $(MAKE) $(MFLAGS)

streaming.so : force_look
	$(ECHO) building streaming : $(MAKE) $(MFLAGS)
	cd streaming; $(MAKE) $(MFLAGS)	


force_look :
	true